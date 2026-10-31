package provider

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

type pveguestProvider struct {
	version string
}

type providerModel struct {
	Nodes       types.Map   `tfsdk:"nodes"`
	MaxRequests types.Int64 `tfsdk:"max_requests"`

	ControllerCAFile types.String `tfsdk:"controller_ca_file"`

	SMTP2GOAPIKey types.String `tfsdk:"smtp2go_api_key"`
	SMTP2GOAPIURL types.String `tfsdk:"smtp2go_api_url"`
}

type nodeModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	APIToken types.String `tfsdk:"api_token"`
	Insecure types.Bool   `tfsdk:"insecure"`
	NodeName types.String `tfsdk:"node_name"`

	MaxExecRequests types.Int64 `tfsdk:"max_exec_requests"`
}

type providerData struct {
	client       *transport.Client
	aptLocks     *guestLocks
	sysrepoLocks *guestLocks

	controllerCAFile string

	smtp2go *smtp2goClient
}

// guestLocks serializes mutations for one guest within this provider instance.
// Separate provider instances do not share these locks.
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
		Description: "Declares files, links, apt packages, systemd units, and sysrepo state inside Proxmox guests. " +
			"Commands run through the Proxmox VE API of the hypervisor.",
		Attributes: map[string]schema.Attribute{
			"nodes": schema.MapNestedAttribute{
				Required: true,
				Description: "Hypervisors by name. A resource selects one with its node argument. " +
					"The map key is the node name in the API path unless the entry sets node_name.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"endpoint": schema.StringAttribute{
							Required:    true,
							Description: "URL of the Proxmox VE API, for example https://10.230.0.254:8006.",
						},
						"api_token": schema.StringAttribute{
							Required:    true,
							Sensitive:   true,
							Description: "API token in the form user@realm!tokenid=secret.",
						},
						"node_name": schema.StringAttribute{
							Optional:    true,
							Description: "Proxmox node name in the API path. The default is the map key.",
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
							},
						},
						"insecure": schema.BoolAttribute{
							Optional:    true,
							Description: "Skips TLS certificate verification. The default is false.",
						},
						"max_exec_requests": schema.Int64Attribute{
							Optional:    true,
							Description: "This setting limits concurrent guest commands for this node configuration. The default is 4, and the minimum is 1.",
							Validators:  []validator.Int64{int64validator.AtLeast(1)},
						},
					},
				},
			},
			"max_requests": schema.Int64Attribute{
				Optional:    true,
				Description: "Concurrent API requests per hypervisor. The default is 4.",
				Validators:  []validator.Int64{int64validator.AtLeast(1)},
			},
			"controller_ca_file": schema.StringAttribute{
				Optional: true,
				Description: "The controller trusts certificate authorities from this PEM file in addition to " +
					"the system roots when a pveguest_download with fetch = \"controller\" or a pveguest_host_artifact downloads a URL.",
			},
			"smtp2go_api_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "The SMTP2GO API key authenticates requests for pveguest_mail_relay. The provider reads PVEGUEST_SMTP2GO_API_KEY when smtp2go_api_key is unset or empty.",
			},
			"smtp2go_api_url": schema.StringAttribute{
				Optional:    true,
				Description: "The SMTP2GO API base URL defaults to https://api.smtp2go.com/v3. The URL must use HTTPS, or HTTP with a loopback host.",
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
	if config.Nodes.IsUnknown() || config.MaxRequests.IsUnknown() {
		resp.Diagnostics.AddError(
			"Unknown provider configuration",
			"The nodes and max_requests arguments must be known when the provider is configured.",
		)
		return
	}
	if config.ControllerCAFile.IsUnknown() {
		resp.Diagnostics.AddError(
			"Unknown provider configuration",
			"The controller_ca_file argument must be known when the provider is configured.",
		)
		return
	}
	if config.SMTP2GOAPIKey.IsUnknown() || config.SMTP2GOAPIURL.IsUnknown() {
		resp.Diagnostics.AddError(
			"Unknown provider configuration",
			"The smtp2go_api_key and smtp2go_api_url arguments must be known when the provider is configured.",
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
		if model.Endpoint.IsUnknown() || model.APIToken.IsUnknown() || model.Insecure.IsUnknown() ||
			model.NodeName.IsUnknown() || model.MaxExecRequests.IsUnknown() {
			resp.Diagnostics.AddError(
				"Unknown provider configuration",
				fmt.Sprintf("The endpoint, api_token, insecure, node_name, and max_exec_requests arguments of node %q must be known when the provider is configured.", name),
			)
			return
		}
		nodes[name] = transport.NodeConfig{
			Endpoint: model.Endpoint.ValueString(),
			APIToken: model.APIToken.ValueString(),
			Insecure: model.Insecure.ValueBool(),
			NodeName: model.NodeName.ValueString(),

			MaxExecRequests: int(model.MaxExecRequests.ValueInt64()),
		}
	}

	maxRequests := transport.DefaultMaxRequests
	if !config.MaxRequests.IsNull() {
		maxRequests = int(config.MaxRequests.ValueInt64())
	}

	client, err := transport.NewClient(nodes, maxRequests)
	if err != nil {
		resp.Diagnostics.AddError("Invalid provider configuration", err.Error())
		return
	}

	smtp2goAPIKey := config.SMTP2GOAPIKey.ValueString()
	if smtp2goAPIKey == "" {
		smtp2goAPIKey = os.Getenv(smtp2goEnvironmentName)
	}
	smtp2goAPIURL := config.SMTP2GOAPIURL.ValueString()
	if smtp2goAPIURL == "" {
		smtp2goAPIURL = defaultSMTP2GOAPIURL
	}
	smtp2go, err := newSMTP2GOClient(smtp2goAPIURL, smtp2goAPIKey)
	if err != nil {
		resp.Diagnostics.AddError("Invalid provider configuration", err.Error())
		return
	}
	data := &providerData{
		client:       client,
		aptLocks:     &guestLocks{locks: make(map[transport.Guest]*sync.Mutex)},
		sysrepoLocks: &guestLocks{locks: make(map[transport.Guest]*sync.Mutex)},

		controllerCAFile: config.ControllerCAFile.ValueString(),

		smtp2go: smtp2go,
	}
	resp.ResourceData = data
	resp.DataSourceData = data
}

func (p *pveguestProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newFileResource,
		newDownloadResource,
		newLinkResource,
		newAptPackagesResource,
		newDebPackagesResource,
		newSystemdUnitResource,
		newLocalesResource,
		newMailRelayResource,
		newSysrepoModuleResource,
		newSysrepoDataResource,
		newHostKernelModulesResource,
		newHostArtifactResource,
		newContainerOptionsResource,
		newContainerPowerResource,
		newVMOptionsResource,
	}
}

func (p *pveguestProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newHostKernelModulesDataSource,
	}
}
