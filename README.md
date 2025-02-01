# Karma Proxy

A cost-effective, scalable reverse proxy and load balancer designed for AWS ECS containers. Karma Proxy provides an alternative to AWS Elastic Load Balancer (ELB), reducing infrastructure costs from ~$25/month to as low as $4/month per backend.

## Features

- Dynamic service discovery for AWS ECS tasks
- Round-robin load balancing
- Redis-based caching for improved performance
- Automatic task health monitoring
- Support for multiple clusters and services
- Periodic cache refresh for service reliability
- Cost-effective alternative to AWS ELB

## How It Works

Karma Proxy operates by creating a dynamic routing layer between your clients and ECS containers. Here's the workflow:

1. **Domain Format**: Services are accessed using the format:
   ```
   servicename-port-cluster.yourdomain.com
   ```
   For example: `backend-8080-production.example.com`

2. **Service Discovery**: The proxy:
   - Queries AWS ECS API to discover running tasks for the requested service
   - Retrieves network interface details to obtain container IP addresses
   - Caches the results in Redis for improved performance

3. **Load Balancing**: 
   - Implements round-robin load balancing across available containers
   - Automatically updates the container pool every 30 seconds
   - Handles both public and private IP addresses

4. **High Performance**:
   - Implements connection timeouts for stability
   - Uses connection pooling for Redis
   - Maintains an in-memory cache with periodic updates

## Prerequisites

- AWS Account with ECS cluster(s)
- Redis instance
- AWS credentials with appropriate permissions for ECS and EC2
- Go 1.x installed for building the proxy

## Configuration

1. Set up the following environment variables:
```env
REDIS_URL="your-redis-url"
AWS_ACCESS_KEY_ID="your-aws-access-key"
AWS_SECRET_ACCESS_KEY="your-aws-secret-key"
AWS_REGION="your-aws-region"
```

2. The proxy listens on port 6969 by default.

## Important Notes

- Service names must be in lowercase
- Container tasks must be in RUNNING state to be included in the load balancer pool
- The proxy attempts to use public IPs first, falling back to private IPs if necessary
- Cache duration is set to 30 seconds by default

## Running the Proxy

1. Build the proxy:
```bash
go build -o karma-proxy
```

2. Run the binary:
```bash
./karma-proxy
```

## Architecture Details

The proxy maintains three main components:

1. **Service Cache**: 
   - Stores service-to-task mappings in Redis
   - Reduces API calls to AWS
   - Automatically refreshes every 30 seconds

2. **Load Balancer**:
   - Maintains current task list per service
   - Implements round-robin selection
   - Thread-safe implementation using sync.Map

3. **Health Checker**:
   - Periodically verifies task health
   - Removes unhealthy tasks from the pool
   - Updates service cache with current state

## Error Handling

- Returns 503 if no healthy tasks are available
- Returns 400 for invalid domain format
- Implements request timeouts (10s read, 10s write)
- Handles AWS API errors with appropriate fallbacks

## Limitations

- Only supports HTTP/HTTPS traffic
- Service names must be lowercase
- No sticky sessions support
- Single region per proxy instance

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
