package driver

import (
	"context"

	"github.com/containerd/nri/pkg/api"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// Driver is the interface that a specific network driver implementation must satisfy.
// The Plugin framework will call these methods at the appropriate times.
type Driver interface {
	// GetDevices returns a list of devices that the driver can manage. This is called
	// periodically by the framework to publish the available resources.
	GetDevices() ([]resourceapi.Device, error)

	// PrepareDevice is called by the framework during the NodePrepareResources hook.
	// It should prepare the device for use by a pod and return any information that
	// is needed by the NRI hooks. This information will be stored in the shared state.
	PrepareDevice(ctx context.Context, claim *resourceapi.ResourceClaim) (interface{}, error)

	// UnprepareDevice is called by the framework during the NodeUnprepareResources hook.
	// It should clean up any resources that were allocated for the device.
	UnprepareDevice(ctx context.Context, claim kubeletplugin.NamespacedObject) error

	// ConfigureDeviceForPod is called by the framework during the RunPodSandbox NRI hook.
	// It should configure the device for use by the pod. The `preparedData` is the
	// information that was returned by PrepareDevice.
	ConfigureDeviceForPod(device AllocatedDevice, networkNamespace string, podSandbox *api.PodSandbox, preparedData interface{}) error

	// CleanupDeviceForPod is called by the framework during the StopPodSandbox NRI hook.
	// It should clean up any resources that were allocated for the device.
	CleanupDeviceForPod(device AllocatedDevice, networkNamespace string, podSandbox *api.PodSandbox, preparedData interface{}) error

	// HandleError is called for errors encountered in the background, for example,
	// while publishing ResourceSlices.
	HandleError(ctx context.Context, err error, msg string)
}

// AllocatedDevice represents a network device that has been allocated to a pod.
type AllocatedDevice struct {
	Name       string
	Attributes map[string]string
	PoolName   string
	Request    string
}

// SharedState is the data that is shared between the DRA and NRI hooks.
// It is managed by the Plugin framework and passed to the driver's hooks.
type SharedState struct {
	// PodDeviceConfig maps a pod's UID to the devices that have been allocated to it.
	PodDeviceConfig map[types.UID][]AllocatedDevice
	// PreparedData maps a pod's UID to the data that was returned by the PrepareDevice hook.
	PreparedData map[types.UID]interface{}
}
