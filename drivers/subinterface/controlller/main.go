package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/aojea/kubernetes-network-drivers/pkg/controller"
	resourcev1 "k8s.io/api/resource/v1"
)

const (
	controllerName = "sample-controller"
	driverName     = "gpu.example.com"
)

//================================================================
// Reconciler Implementation
//================================================================

// sampleReconciler implements the controller.Reconciler interface.
type sampleReconciler struct {
	kubeClient kubernetes.Interface
}

// IsDeviceClassRelevant checks if this controller should handle the claim.
func (c *sampleReconciler) IsDeviceClassRelevant(deviceClass *resourcev1.DeviceClass) bool {
	return deviceClass.Name == driverName
}

// Reconcile creates a ResourceSlice for a virtual GPU.
func (c *sampleReconciler) Reconcile(ctx context.Context, claim *resourcev1.ResourceClaim) (*resourcev1.ResourceSlice, error) {
	sliceName := c.getResourceSliceName(claim)

	virtualDevice := resourcev1.Device{
		Name: fmt.Sprintf("virtual-gpu-%s", claim.UID),
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"type": {StringValue: func() *string { s := "virtual-gpu"; return &s }()},
		},
		Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
			"memory": {Value: resource.MustParse("4Gi")},
		},
	}

	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: sliceName,
		},
		Spec: resourcev1.ResourceSliceSpec{
			NodeName: ptr.To("node0"),
			Driver:   driverName,
			Pool: resourcev1.ResourcePool{
				Name:               "virtual-gpus",
				Generation:         1,
				ResourceSliceCount: 1,
			},
			Devices: []resourcev1.Device{virtualDevice},
		},
	}, nil
}

// Delete removes the ResourceSlice associated with a ResourceClaim.
func (c *sampleReconciler) Delete(ctx context.Context, claim *resourcev1.ResourceClaim) error {
	sliceName := c.getResourceSliceName(claim)
	klog.Infof("Deleting ResourceSlice %s", sliceName)
	err := c.kubeClient.ResourceV1().ResourceSlices().Delete(ctx, sliceName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete ResourceSlice %s: %w", sliceName, err)
	}
	klog.Infof("Successfully deleted ResourceSlice %s", sliceName)
	return nil
}

// getResourceSliceName generates a deterministic name for the ResourceSlice.
func (c *sampleReconciler) getResourceSliceName(claim *resourcev1.ResourceClaim) string {
	return fmt.Sprintf("%s-%s", controllerName, claim.UID)
}

//================================================================
// Main Entrypoint
//================================================================

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer cancel()

	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// 1. Create an instance of your reconciler implementation
	myReconciler := &sampleReconciler{
		kubeClient: clientset,
	}

	// 2. Create and run the controller
	ctrl := controller.NewController(clientset, myReconciler, controllerName)
	ctrl.Run(ctx)
}
