package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/aojea/kubernetes-network-drivers/pkg/driver"
	kndnet "github.com/aojea/kubernetes-network-drivers/pkg/net"
)

//================================================================
// Driver Implementation
//================================================================

// hostdeviceDriver implements the driver.Driver interface.
type hostdeviceDriver struct{}

// NewDriver creates a new instance of the hostdevice driver.
func NewDriver() driver.Driver {
	return &hostdeviceDriver{}
}

// GetDevices discovers all physical network interfaces on the host.
func (d *hostdeviceDriver) GetDevices() ([]resourceapi.Device, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list network interfaces: %w", err)
	}

	var devices []resourceapi.Device
	for _, link := range links {
		attrs := link.Attrs()

		// Skip loopback, virtual, and down interfaces
		if attrs.Flags&net.FlagLoopback != 0 || attrs.Flags&net.FlagUp == 0 {
			continue
		}
		if strings.HasPrefix(attrs.Name, "veth") || strings.HasPrefix(attrs.Name, "docker") || strings.HasPrefix(attrs.Name, "cni") {
			continue
		}

		device := resourceapi.Device{
			Name: attrs.Name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"interface-name": {StringValue: &attrs.Name},
				"mac-address":    {StringValue: func() *string { s := attrs.HardwareAddr.String(); return &s }()},
			},
		}
		devices = append(devices, device)
		klog.V(2).Infof("Discovered device: %s", attrs.Name)
	}
	return devices, nil
}

// PrepareDevice extracts the target interface name from the claim.
func (d *hostdeviceDriver) PrepareDevice(ctx context.Context, claim *resourceapi.ResourceClaim) (interface{}, error) {
	if claim.Status.Allocation == nil || len(claim.Status.Allocation.Devices.Results) == 0 {
		return nil, fmt.Errorf("claim %s has no allocated devices", claim.Name)
	}

	// For this simple driver, we just need the name of the device to move.
	// The device name is the primary information we need for ConfigureDeviceForPod.
	deviceName := claim.Status.Allocation.Devices.Results[0].Device
	klog.Infof("Preparing device %q for claim %s", deviceName, claim.Name)

	return deviceName, nil
}

// UnprepareDevice is a no-op for this simple driver.
func (d *hostdeviceDriver) UnprepareDevice(ctx context.Context, claim kubeletplugin.NamespacedObject) error {
	klog.Infof("Unpreparing resources for claim %s", claim.Name)
	return nil
}

// ConfigureDeviceForPod moves the allocated network device into the pod's namespace.
func (d *hostdeviceDriver) ConfigureDeviceForPod(device driver.AllocatedDevice, networkNamespace string, podSandbox *api.PodSandbox, preparedData interface{}) error {
	hostDeviceName, ok := preparedData.(string)
	if !ok {
		return fmt.Errorf("invalid prepared data type: expected string, got %T", preparedData)
	}

	// The device name inside the pod will be the same as on the host.
	podInterfaceName := hostDeviceName

	klog.Infof("Moving device %q to pod %s/%s network namespace %s as %q",
		hostDeviceName, podSandbox.Namespace, podSandbox.Name, networkNamespace, podInterfaceName)

	// Here we use the plumbing library to do the actual work.
	_, err := kndnet.NsAttachNetdev(hostDeviceName, networkNamespace, netlink.LinkAttrs{Name: podInterfaceName}, nil)
	return err
}

// CleanupDeviceForPod moves the network device back to the host namespace.
func (d *hostdeviceDriver) CleanupDeviceForPod(device driver.AllocatedDevice, networkNamespace string, podSandbox *api.PodSandbox, preparedData interface{}) error {
	hostDeviceName, ok := preparedData.(string)
	if !ok {
		return fmt.Errorf("invalid prepared data type: expected string, got %T", preparedData)
	}

	podInterfaceName := hostDeviceName

	klog.Infof("Moving device %q from pod %s/%s back to host namespace",
		podInterfaceName, podSandbox.Namespace, podSandbox.Name)

	// Use the plumbing library to move the device back.
	return kndnet.NsDetachNetdev(networkNamespace, podInterfaceName, hostDeviceName)
}

// HandleError logs background errors.
func (d *hostdeviceDriver) HandleError(ctx context.Context, err error, msg string) {
	klog.Errorf("background error: %s: %v", msg, err)
}

//================================================================
// Main Entrypoint
//================================================================

const (
	driverName = "hostdevice.k8s.io"
)

var (
	hostnameOverride string
	kubeconfig       string
	bindAddress      string
	ready            atomic.Bool
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&bindAddress, "bind-address", ":9177", "The IP address and port for the metrics and healthz server to serve on")
	flag.StringVar(&hostnameOverride, "hostname-override", "", "If non-empty, will be used as the name of the Node is running on.")
	klog.InitFlags(nil)
	flag.Parse()
}

func main() {
	printVersion()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer cancel()

	// Set up healthz and metrics endpoints
	setupHTTPServer()

	// Set up Kubernetes client
	clientset, err := newClientset()
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	nodeName, err := nodeutil.GetHostname(hostnameOverride)
	if err != nil {
		klog.Fatalf("Cannot get node name: %v", err)
	}

	// 1. Create an instance of the driver
	hdDriver := NewDriver()

	// 2. Create the plugin, passing in your driver instance
	plugin := driver.NewPlugin(hdDriver, driverName, nodeName, clientset)

	// 3. Start the plugin
	if err := plugin.Start(ctx); err != nil {
		klog.Fatalf("Driver failed to start: %v", err)
	}
	defer plugin.Stop()

	ready.Store(true)
	klog.Infof("Driver started successfully on node %s", nodeName)

	// Wait for termination signal
	<-ctx.Done()
	klog.Info("Driver shutting down")
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
	info, _ := debug.ReadBuildInfo()
	klog.Infof("Version: %s, Go version: %s", info.Main.Version, info.GoVersion)
}
