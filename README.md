# Fargate calculator

Calculate potential cost of AWS Fargate based on current EKS / Kubernetes workload.

```shell
$ go install https://github.com/tscn/fargate-calculator

$ fargate-calculator --help
Usage: fargate-calculator

Calculate Fargate cost for Kubernetes workload.

Flags:
  -h, --help                                 Show context-sensitive help.
      --namespace=""                         Namespace selector (optional)
      --fargate-cpu-hour=0.04656             Price of Fargate CPU per Hour
      --fargate-memory-hour=0.00511          Price of Fargate Memory per Hour
      --ec2-instance-hour=c5.xlarge=0.194    Hourly prices of instance types (comma-separated), e.g. c5.xlarge=0.194
      --exclude-daemonsets                   Exclude Pods owned by DaemonSets (as not supported in Fargate).
      --exclude-istio-proxy                  Exclude istio-proxy containers (as not supported in Fargate).
      --debug                                Enable debug logging.
```