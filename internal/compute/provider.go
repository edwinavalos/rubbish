package compute

import "context"

type ComputeProvider interface {
	CreateSandbox(ctx context.Context, req CreateSandboxRequest) (SandboxHandle, error)
	StartSandbox(ctx context.Context, handle SandboxHandle) (NetworkAttachment, error)
	StopSandbox(ctx context.Context, handle SandboxHandle) error
	DestroySandbox(ctx context.Context, handle SandboxHandle) error
	DescribeSandbox(ctx context.Context, handle SandboxHandle) (SandboxStatus, error)
	Name() string
}

type CreateSandboxRequest struct {
	SessionID    string
	ImageRef     string
	Resources    SandboxResources
	WorktreePath string
	Metadata     map[string]string
}

type SandboxHandle struct {
	ID           string
	ProviderName string
	ProviderData any
}

type SandboxResources struct {
	VCPUs     int64
	MemoryMiB int64
}

type SandboxStatus struct {
	State   SandboxState
	Message string
}

type SandboxState int

const (
	SandboxStateUnspecified SandboxState = iota
	SandboxStateCreating
	SandboxStateRunning
	SandboxStateStopping
	SandboxStateStopped
	SandboxStateFailed
)

type NetworkAttachment struct {
	ReachableAt  string // e.g. "172.16.0.2:22"
	GatewayIP    string // e.g. "172.16.0.1"
	DNS          []string
	ProviderMeta any
}
