package main

import (
	"context"
	"github.com/alecthomas/kong"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"path/filepath"
)

func main() {
	ctx := kong.Parse(&Interface{},
		kong.Name("fargate-calculator"),
		kong.Description("Calculate Fargate cost for Kubernetes workload."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{
			Compact: true,
			Summary: false,
		}))

	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}

type Interface struct {
	Namespace          string             `name:"namespace" help:"Namespace selector (optional)" default:""`
	UseRequestsOnly    bool               `name:"use-requests-only" help:"If set to true, calculator will only use requests and not limits." default:"false"`
	AssumeOptimization bool               `name:"assume-request-optimization" help:"Enabling this option will make calculator expect that requests would be adjusted down to meet Fargate pod config values." default:"false"`
	FargateCPUHour     float64            `name:"fargate-cpu-hour" help:"Price of Fargate CPU per Hour" default:"0.04656"`
	FargateMemoryHour  float64            `name:"fargate-memory-hour" help:"Price of Fargate Memory per Hour" default:"0.00511"`
	Ec2InstanceHour    map[string]float64 `name:"ec2-instance-hour" help:"Hourly prices of instance types (comma-separated), e.g. c5.xlarge=0.194" default:"c5.xlarge=0.194"`
	ExcludeDaemonSets  bool               `name:"exclude-daemonsets" help:"Exclude Pods owned by DaemonSets (as not supported in Fargate)." default:"true"`
	ExcludeIstioProxy  bool               `name:"exclude-istio-proxy" help:"Exclude istio-proxy containers (as not supported in Fargate)." default:"true"`
	Debug              bool               `name:"debug" help:"Enable debug logging."`
}

func (cmd *Interface) Run() error {
	if cmd.Debug {
		log.SetLevel(log.DebugLevel)
	}
	ctx := context.TODO()

	clientSet, err := getClientSet()
	podList, err := clientSet.CoreV1().Pods(cmd.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	log.Debugf("Found %v pods.", len(podList.Items))

	var fargateMegaPerMillis = getFargateMegaPerMillis()

	var fargateTotalCpu int64
	var fargateTotalMemory int64
	var fargateTotalPrice float64
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodSucceeded && pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if cmd.ExcludeDaemonSets {
			var isDaemonSetPod = false
			for _, owner := range pod.OwnerReferences {
				if owner.Kind == "DaemonSet" {
					isDaemonSetPod = true
				}
			}
			if isDaemonSetPod {
				log.Debugf("Skipping DaemonSet Pod %s/%s.", pod.Namespace, pod.Name)
				continue
			}
		}

		var podCpu, podMemory resource.Quantity
		for _, container := range pod.Spec.Containers {
			if cmd.ExcludeIstioProxy && container.Name == "istio-proxy" {
				continue
			}
			if container.Resources.Limits.Cpu().IsZero() || cmd.UseRequestsOnly {
				if !container.Resources.Requests.Cpu().IsZero() {
					podCpu.Add(*container.Resources.Requests.Cpu())
				}
			} else {
				podCpu.Add(*container.Resources.Limits.Cpu())
			}

			if container.Resources.Limits.Memory().IsZero() || cmd.UseRequestsOnly {
				if !container.Resources.Requests.Memory().IsZero() {
					podMemory.Add(*container.Resources.Requests.Memory())
				}
			} else {
				podMemory.Add(*container.Resources.Limits.Memory())
			}
		}

		podMemory.Add(*resource.NewScaledQuantity(250, resource.Mega))
		//log.Debugf("Caluclated pod %s/%s with %vm CPU and %vMi memory.", pod.Namespace, pod.Name, podCpu.ScaledValue(resource.Milli), podMemory.ScaledValue(resource.Mega))

		if cmd.AssumeOptimization {
			if podCpu.ScaledValue(resource.Milli) > 1500 {
				podCpu.Sub(*resource.NewScaledQuantity(1000, resource.Milli))
			} else if podCpu.ScaledValue(resource.Milli) > 750 {
				podCpu.Sub(*resource.NewScaledQuantity(500, resource.Milli))
			} else {
				podCpu.Sub(*resource.NewScaledQuantity(250, resource.Milli))
			}
			if podMemory.ScaledValue(resource.Mega) > 1536 {
				podMemory.Sub(*resource.NewScaledQuantity(1024, resource.Mega))
			} else {
				podMemory.Sub(*resource.NewScaledQuantity(512, resource.Mega))
			}
		}

		var match = false
		for _, cpuOption := range getFargateMillis() {
			if podCpu.IsZero() || cpuOption >= podCpu.ScaledValue(resource.Milli) {
				for _, memoryOption := range fargateMegaPerMillis[cpuOption] {
					if memoryOption >= podMemory.ScaledValue(resource.Mega) {
						match = true
						var fargatePrice = (float64(cpuOption) / 1000 * cmd.FargateCPUHour) + (float64(memoryOption) / 1024 * cmd.FargateMemoryHour)
						log.Infof("Resolved Fargate configuration %v CPU and %v Memory for Pod %s/%s (%vm / %vMi) with hourly price: %v$", float64(cpuOption)/1000, float64(memoryOption)/1024, pod.Namespace, pod.Name, podCpu.ScaledValue(resource.Milli), podMemory.ScaledValue(resource.Mega), fargatePrice)
						fargateTotalCpu += cpuOption
						fargateTotalMemory += memoryOption
						fargateTotalPrice += fargatePrice
						break
					}
				}
				if match {
					break
				}
			}
		}
		if !match {
			log.Warnf("Did not match a fargate config for pod %s/%s with cpu %vm and memory %vMi.", pod.Namespace, pod.Name, podCpu.ScaledValue(resource.Milli), podMemory.ScaledValue(resource.Mega))
		}
	}

	log.Infof("Total Fargate price/h for pods: %f", fargateTotalPrice)

	nodeList, err := clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	log.Debugf("Found %v nodes.", len(nodeList.Items))

	var ec2Price float64

	var fargateEquivalent float64
	for _, node := range nodeList.Items {
		if instanceType, ok := node.Labels["node.kubernetes.io/instance-type"]; ok {
			if price, ok := cmd.Ec2InstanceHour[instanceType]; ok {
				ec2Price = ec2Price + price
			} else {
				log.Warnf("EC2 price for %s not provided.", instanceType)
			}
		} else {
			log.Warnf("Cannot determine instance type for node %s", node.Name)
		}
		fargateEquivalent += float64(node.Status.Allocatable.Cpu().ScaledValue(resource.Milli)) / 1000 * cmd.FargateCPUHour
		fargateEquivalent += float64(node.Status.Allocatable.Memory().ScaledValue(resource.Mega)) / 1024 * cmd.FargateMemoryHour
	}

	log.Infof("Total EC2 price/h for nodes: %v", ec2Price)
	log.Infof("Fargate price/h for equivalent allocatable ressources: %v", fargateEquivalent)
	return nil
}

func getFargateMillis() []int64 {
	return []int64{250, 500, 1000, 2000, 4000, 8000, 16000}
}

func getFargateMegaPerMillis() map[int64][]int64 {
	var result = map[int64][]int64{
		250:   []int64{512, 1024, 2048},
		500:   []int64{1024, 2048, 3072, 4096},
		1000:  make([]int64, 0),
		2000:  make([]int64, 0),
		4000:  make([]int64, 0),
		8000:  make([]int64, 0),
		16000: make([]int64, 0),
	}
	for i := 2; i <= 8; i++ {
		result[1000] = append(result[1000], int64(i*1024))
	}
	for i := 4; i <= 16; i++ {
		result[2000] = append(result[2000], int64(i*1024))
	}
	for i := 8; i <= 30; i++ {
		result[4000] = append(result[4000], int64(i*1024))
	}
	for i := 16; i <= 60; i += 4 {
		result[8000] = append(result[8000], int64(i*1024))
	}
	for i := 32; i <= 120; i += 8 {
		result[16000] = append(result[16000], int64(i*1024))
	}
	return result

}

func getClientSet() (*kubernetes.Clientset, error) {
	kubeConfig := filepath.Join(
		os.Getenv("HOME"), ".kube", "config",
	)
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(config)
}
