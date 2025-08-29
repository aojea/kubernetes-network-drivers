package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sys/unix"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/aojea/kubernetes-network-drivers/pkg/driver"
)

//================================================================
// Agent (Driver) Implementation
//================================================================

const (
	// The resourceFile simulates a unique hardware or software feature on the node.
	// If this file exists, the agent advertises the resource.
	resourceFile = "/tmp/special-node-resource"
)

// nodeAgentDriver implements the driver.Driver interface.
type nodeAgentDriver struct{}

// NewDriver creates a new instance of the node agent driver.
func NewDriver() driver.Driver {
	return &nodeAgentDriver{}
}

// GetDevices checks for the presence of the special resource file and advertises it as a device.
func (d *nodeAgentDriver) GetDevices() ([]resourcev1.Device, error) {
	if _, err := os.Stat(resourceFile); os.IsNotExist(err) {
		klog.V(2).Infof("Special resource file not found at %s. No devices to advertise.", resourceFile)
		return nil, nil
	}

	klog.V(1).Infof("Discovered special resource at %s", resourceFile)
	device := resourcev1.Device{
		Name: "special-resource-0",
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"feature": {StringValue: func() *string { s := "enabled"; return &s }()},
		},
	}
	return []resourcev1.Device{device}, nil
}

// PrepareDevice simulates claiming the resource by writing the pod's UID to the file.
func (d *nodeAgentDriver) PrepareDevice(ctx context.Context, claim *resourcev1.ResourceClaim) (interface{}, error) {
	podUID := string(claim.UID)
	klog.Infof("Preparing special resource for claim %s (Pod UID: %s)", claim.Name, podUID)

	// "Claim" the resource by writing the UID of the pod to the file.
	err := os.WriteFile(resourceFile, []byte(podUID), 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to write pod UID to resource file: %w", err)
	}

	return podUID, nil
}

// UnprepareDevice simulates releasing the resource.
func (d *nodeAgentDriver) UnprepareDevice(ctx context.Context, claim kubeletplugin.NamespacedObject) error {
	klog.Infof("Unpreparing special resource for claim %s", claim.Name)
	// Clear the file to indicate the resource is free.
	return os.WriteFile(resourceFile, []byte(""), 0644)
}

// ConfigureDeviceForPod is a no-op for this agent, as the pod would typically mount the
// resource file directly to see that it has been allocated the resource.
func (d *nodeAgentDriver) ConfigureDeviceForPod(device driver.AllocatedDevice, networkNamespace string, podSandbox *api.PodSandbox, preparedData interface{}) error {
	klog.Infof("No-op configuration for pod %s/%s. Pod can access resource via mounted file.", podSandbox.Namespace, podSandbox.Name)
	return nil
}

// CleanupDeviceForPod is a no-op. The resource is released in UnprepareDevice.
func (d *nodeAgentDriver) CleanupDeviceForPod(device driver.AllocatedDevice, networkNamespace string, podSandbox *api.PodSandbox, preparedData interface{}) error {
	klog.Infof("No-op cleanup for pod %s/%s.", podSandbox.Namespace, podSandbox.Name)
	return nil
}

// HandleError logs background errors from the driver framework.
func (d *nodeAgentDriver) HandleError(ctx context.Context, err error, msg string) {
	klog.Errorf("Background error in driver framework: %s: %v", msg, err)
}

//================================================================
// Main Entrypoint
//================================================================

const (
	driverName = "node-agent.k8s.io"
)

var (
	hostnameOverride string
	kubeconfig       string
	bindAddress      string
	ready            atomic.Bool
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Absolute path to the kubeconfig file.")
	flag.StringVar(&bindAddress, "bind-address", ":9178", "The IP address and port for the metrics and healthz server.")
	flag.StringVar(&hostnameOverride, "hostname-override", "", "Node name to use. Defaults to the node's hostname.")
	klog.InitFlags(nil)
}

func main() {
	flag.Parse()
	printVersion()

	// Create a context that is cancelled on interruption signals.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer cancel()

	// Set up healthz and metrics endpoints.
	setupHTTPServer()

	// Set up Kubernetes client.
	clientset, err := newClientset()
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	nodeName, err := nodeutil.GetHostname(hostnameOverride)
	if err != nil {
		klog.Fatalf("Cannot get node name: %v", err)
	}

	// 1. Create an instance of the node agent driver.
	nodeAgent := NewDriver()

	// 2. Create the plugin framework, passing in the agent.
	plugin := driver.NewPlugin(nodeAgent, driverName, nodeName, clientset)

	// 3. Start the plugin framework.
	if err := plugin.Start(ctx); err != nil {
		klog.Fatalf("Node agent failed to start: %v", err)
	}
	defer plugin.Stop()

	ready.Store(true)
	klog.Infof("Node agent started successfully on node %s", nodeName)

	// Wait for the context to be cancelled.
	<-ctx.Done()
	klog.Info("Node agent shutting down.")
}

func setupHTTPServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.Handle("/metrics", promhttp.Handler())
	server := &http.Server{Addr: bindAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Failed to listen and serve: %v", err)
		}
	}()
}

func newClientset() (kubernetes.Interface, error) {
	var config *rest.Config
	var err error
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("cannot create client-go config: %w", err)
	}
	return kubernetes.NewForConfig(config)
}

func printVersion() {
	if info, ok := debug.ReadBuildInfo(); ok {
		klog.Infof("Version: %s, Go version: %s", info.Main.Version, info.GoVersion)
	}
}
