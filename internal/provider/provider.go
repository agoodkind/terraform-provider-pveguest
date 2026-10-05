package provider

import (
	"context"
	"fmt"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const (
	minimumPort = 1
	maximumPort = 65535
)

type pveguestProvider struct {
	version string
}

type providerModel struct {
	Nodes       types.Map   `tfsdk:"nodes"`
	MaxSessions types.Int64 `tfsdk:"max_sessions"`
}

type nodeModel struct {
	Host types.String `tfsdk:"host"`
	Port types.Int64  `tfsdk:"port"`
	User types.String `tfsdk:"user"`
}

type providerData struct {
	pool     *transport.Pool
	aptLocks *guestLocks
}

// guestLocks serializes operations per guest. apt and dpkg use one lock
// file per guest and fail when two processes run at once.
type guestLocks struct {
	mutex sync.Mutex
	locks map[transport.Guest]*sync.Mutex
}

func (locks *guestLocks) lock(guest transport.Guest) func() {
	locks.mutex.Lock()
	guestLock, found := locks.locks[guest]
	if !found {
		guestLock = &sync.Mutex{}
		locks.locks[guest] = guestLock
	}
	locks.mutex.Unlock()

	guestLock.Lock()
	return guestLock.Unlock
}

// New returns the factory of the pveguest provider. OpenTofu receives version
// as the provider version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &pveguestProvider{version: version}
	}
}

func (p *pveguestProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "pveguest"
	resp.Version = p.version
}

func (p *pveguestProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Declares files, links, apt packages, and systemd units inside Proxmox guests. " +
			"Commands run through pct exec or qm guest exec on the hypervisor.",
		Attributes: map[string]schema.Attribute{
			"nodes": schema.MapNestedAttribute{
				Required:    true,
				Description: "Hypervisors by node name. A resource selects one with its node argument.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"host": schema.StringAttribute{
							Required:    true,
							Description: "SSH host name or address of the hypervisor.",
						},
						"port": schema.Int64Attribute{
							Optional:    true,
							Description: "SSH port. The default is 22.",
							Validators:  []validator.Int64{int64validator.Between(minimumPort, maximumPort)},
						},
						"user": schema.StringAttribute{
							Optional:    true,
							Description: "SSH user. The default is root.",
						},
					},
				},
			},
			"max_sessions": schema.Int64Attribute{
				Optional:    true,
				Description: "Concurrent SSH sessions per hypervisor. The default is 4.",
				Validators:  []validator.Int64{int64validator.AtLeast(1)},
			},
		},
	}
}

func (p *pveguestProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if config.Nodes.IsUnknown() || config.MaxSessions.IsUnknown() {
		resp.Diagnostics.AddError(
			"Unknown provider configuration",
			"The nodes and max_sessions arguments must be known when the provider is configured.",
		)
		return
	}

	nodeModels := make(map[string]nodeModel, len(config.Nodes.Elements()))
	resp.Diagnostics.Append(config.Nodes.ElementsAs(ctx, &nodeModels, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	nodes := make(map[string]transport.NodeConfig, len(nodeModels))
	for name, model := range nodeModels {
		if model.Host.IsUnknown() || model.Port.IsUnknown() || model.User.IsUnknown() {
			resp.Diagnostics.AddError(
				"Unknown provider configuration",
				fmt.Sprintf("The host, port, and user of node %q must be known when the provider is configured.", name),
			)
			return
		}
		nodeConfig := transport.NodeConfig{
			Host: model.Host.ValueString(),
			Port: transport.DefaultPort,
			User: transport.DefaultUser,
		}
		if !model.Port.IsNull() {
			nodeConfig.Port = int(model.Port.ValueInt64())
		}
		if !model.User.IsNull() {
			nodeConfig.User = model.User.ValueString()
		}
		nodes[name] = nodeConfig
	}

	maxSessions := transport.DefaultMaxSessions
	if !config.MaxSessions.IsNull() {
		maxSessions = int(config.MaxSessions.ValueInt64())
	}

	pool, err := transport.NewPool(nodes, maxSessions)
	if err != nil {
		resp.Diagnostics.AddError("Invalid provider configuration", err.Error())
		return
	}
	resp.ResourceData = &providerData{
		pool:     pool,
		aptLocks: &guestLocks{locks: make(map[transport.Guest]*sync.Mutex)},
	}
}

func (p *pveguestProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newFileResource,
		newLinkResource,
		newAptPackagesResource,
		newSystemdUnitResource,
	}
}

func (p *pveguestProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}
