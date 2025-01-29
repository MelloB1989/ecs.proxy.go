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
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/redis/go-redis/v9"
)

type ProxyServer struct {
	rdb           *redis.Client
	ecsClient     *ecs.Client
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
		loadBalancer:  &sync.Map{},
		cacheDuration: 30 * time.Second,
	}, nil
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
		return nil, fmt.Errorf("failed to list tasks: %v", err)
	}

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

	// Extract private IPs
	var taskIPs []string
	for _, task := range taskDetails.Tasks {
		for _, container := range task.Containers {
			if container.NetworkInterfaces != nil && len(container.NetworkInterfaces) > 0 {
				taskIPs = append(taskIPs, *container.NetworkInterfaces[0].PrivateIpv4Address)
			}
		}
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
		http.Error(w, "Invalid domain format", http.StatusBadRequest)
		return
	}

	serviceName := parts[0]
	port := parts[1]
	cluster := parts[2]

	// Get target using round-robin
	serviceInfo, err := p.getNextTarget(serviceName, cluster)
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
	karmaConfig := c.DefaultConfig()
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
