package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	c "github.com/MelloB1989/karma/config"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/redis/go-redis/v9"
)

type ProxyServer struct {
	rdb           *redis.Client
	ecsClient     *ecs.Client
	ec2Client     *ec2.Client
	loadBalancer  *sync.Map
	cacheDuration time.Duration
}

type ServiceInfo struct {
	Tasks     []string
	LastIndex int
}

func NewProxyServer(redisAddr string) (*ProxyServer, error) {
	// Initialize Redis client
	rdb := redis.NewClient(&redis.Options{
		Addr:         redisAddr,
		Password:     "",
		DB:           0,
		PoolSize:     100,
		MinIdleConns: 10,
	})

	// Initialize AWS config
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %v", err)
	}

	return &ProxyServer{
		rdb:           rdb,
		ecsClient:     ecs.NewFromConfig(cfg),
		ec2Client:     ec2.NewFromConfig(cfg),
		loadBalancer:  &sync.Map{},
		cacheDuration: 30 * time.Second,
	}, nil
}

func (p *ProxyServer) getPublicIPFromENI(ctx context.Context, eniID string) (string, error) {
	input := &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []string{eniID},
	}

	result, err := p.ec2Client.DescribeNetworkInterfaces(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to describe network interface: %v", err)
	}

	if len(result.NetworkInterfaces) > 0 && result.NetworkInterfaces[0].Association != nil {
		return *result.NetworkInterfaces[0].Association.PublicIp, nil
	}

	return "", fmt.Errorf("no public IP found for ENI")
}

func (p *ProxyServer) getServiceTasks(ctx context.Context, serviceName, cluster string) (*ServiceInfo, error) {
	// Try to get from Redis first
	cacheKey := fmt.Sprintf("service:%s:cluster:%s:tasks", serviceName, cluster)
	if cached, err := p.rdb.Get(ctx, cacheKey).Result(); err == nil {
		var serviceInfo ServiceInfo
		if err := json.Unmarshal([]byte(cached), &serviceInfo); err == nil {
			return &serviceInfo, nil
		}
	}

	// If not in cache, fetch from ECS
	input := &ecs.ListTasksInput{
		Cluster:     &cluster,
		ServiceName: &serviceName,
	}

	tasks, err := p.ecsClient.ListTasks(ctx, input)
	if err != nil {
		log.Printf("ListTasks error: %v", err)
		return nil, fmt.Errorf("failed to list tasks: %v", err)
	}

	log.Printf("Found %d tasks", len(tasks.TaskArns))

	if len(tasks.TaskArns) == 0 {
		return nil, fmt.Errorf("no tasks found for service %s in cluster %s", serviceName, cluster)
	}

	// Get task details
	describeInput := &ecs.DescribeTasksInput{
		Cluster: &cluster,
		Tasks:   tasks.TaskArns,
	}

	taskDetails, err := p.ecsClient.DescribeTasks(ctx, describeInput)
	if err != nil {
		return nil, fmt.Errorf("failed to describe tasks: %v", err)
	}

	// Add this debugging code temporarily
	for _, task := range taskDetails.Tasks {
		taskJSON, _ := json.MarshalIndent(task, "", "    ")
		log.Printf("Task details: %s", string(taskJSON))
	}

	// Extract public IPs from attachments
	var taskIPs []string
	for _, task := range taskDetails.Tasks {
		if task.LastStatus != nil && *task.LastStatus == "RUNNING" {
			// Get ENI ID from attachment
			var eniID string
			for _, attachment := range task.Attachments {
				if *attachment.Type == "ElasticNetworkInterface" {
					for _, detail := range attachment.Details {
						if *detail.Name == "networkInterfaceId" {
							eniID = *detail.Value
							break
						}
					}
				}
			}

			if eniID != "" {
				// Get public IP using EC2 API
				if publicIP, err := p.getPublicIPFromENI(ctx, eniID); err == nil {
					taskIPs = append(taskIPs, publicIP)
					log.Printf("Added public IP: %s for ENI: %s", publicIP, eniID)
					continue
				} else {
					log.Printf("Failed to get public IP for ENI %s: %v", eniID, err)
				}
			}

			// Fallback to private IP if no public IP found
			for _, container := range task.Containers {
				for _, ni := range container.NetworkInterfaces {
					if ni.PrivateIpv4Address != nil {
						taskIPs = append(taskIPs, *ni.PrivateIpv4Address)
						log.Printf("Added private IP (fallback): %s for task: %s", *ni.PrivateIpv4Address, *task.TaskArn)
					}
				}
			}
		}
	}

	if len(taskIPs) == 0 {
		return nil, fmt.Errorf("no IPs found for running tasks in service %s", serviceName)
	}

	serviceInfo := &ServiceInfo{
		Tasks:     taskIPs,
		LastIndex: 0,
	}

	// Cache the result
	if cached, err := json.Marshal(serviceInfo); err == nil {
		p.rdb.Set(ctx, cacheKey, string(cached), p.cacheDuration)
	}

	return serviceInfo, nil
}

func (p *ProxyServer) getNextTarget(serviceName, cluster string) (*ServiceInfo, error) {
	cacheKey := fmt.Sprintf("%s:%s", cluster, serviceName)
	val, _ := p.loadBalancer.LoadOrStore(cacheKey, &ServiceInfo{})
	serviceInfo := val.(*ServiceInfo)

	if len(serviceInfo.Tasks) == 0 {
		// Fetch new tasks
		ctx := context.Background()
		newInfo, err := p.getServiceTasks(ctx, serviceName, cluster)
		if err != nil {
			return nil, err
		}
		p.loadBalancer.Store(cacheKey, newInfo)
		return newInfo, nil
	}

	return serviceInfo, nil
}

func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Parse domain parts
	parts := strings.Split(r.Host, ".")
	if len(parts) < 6 {
		log.Printf("Invalid domain format. Got %d parts, expected at least 6", len(parts))
		http.Error(w, "Invalid domain format", http.StatusBadRequest)
		return
	}

	serviceName := parts[0]
	port := parts[1]
	cluster := parts[2]

	log.Printf("Parsed request - Service: %s, Port: %s, Cluster: %s", serviceName, port, cluster)

	// Get target using round-robin
	serviceInfo, err := p.getNextTarget(serviceName, cluster)
	log.Println(err)
	if err != nil {
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}

	// Round-robin selection
	currentIndex := serviceInfo.LastIndex
	serviceInfo.LastIndex = (currentIndex + 1) % len(serviceInfo.Tasks)
	targetIP := serviceInfo.Tasks[currentIndex]

	// Create target URL
	target := fmt.Sprintf("http://%s:%s", targetIP, port)
	targetURL, err := url.Parse(target)
	if err != nil {
		http.Error(w, "Invalid target URL", http.StatusInternalServerError)
		return
	}

	// Create reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.ServeHTTP(w, r)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("Starting proxy server...")

	karmaConfig := c.DefaultConfig()
	log.Printf("Redis URL: %s", karmaConfig.RedisURL)

	proxy, err := NewProxyServer(karmaConfig.RedisURL)
	if err != nil {
		log.Fatalf("Failed to create proxy server: %v", err)
	}

	// Refresh task cache periodically
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		for range ticker.C {
			proxy.loadBalancer.Range(func(key, value interface{}) bool {
				cacheKey := key.(string)
				parts := strings.Split(cacheKey, ":")
				cluster, serviceName := parts[0], parts[1]

				ctx := context.Background()
				if info, err := proxy.getServiceTasks(ctx, serviceName, cluster); err == nil {
					proxy.loadBalancer.Store(cacheKey, info)
				}
				return true
			})
		}
	}()

	port := ":6969"

	server := &http.Server{
		Addr:    port,
		Handler: proxy,
		// Timeouts to prevent slow clients from holding connections
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		IdleTimeout:    30 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1MB
	}

	log.Println("Server up and running on", port)

	log.Fatal(server.ListenAndServe())
}
