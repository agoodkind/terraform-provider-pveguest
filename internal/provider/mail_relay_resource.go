package provider

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"path"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	fwpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	defaultRelayFilePath = "/etc/msmtprc"
	relayFileMode        = "0600"

	relayPasswordLength   = 32
	relayPasswordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

	minimumRelayPort = 1
	maximumRelayPort = 65535

	writeRelaySummary       = "Mail relay configuration failed"
	readRelaySummary        = "Mail relay file read failed"
	relayUserUnreadSummary  = "SMTP2GO user query failed"
	relayUserMissingSummary = "SMTP2GO user is absent"
	relayUserBlockedSummary = "SMTP2GO user status is not allowed"

	msmtprcFormat = `defaults
auth           on
tls            on
tls_starttls   on
tls_trust_file /etc/ssl/certs/ca-certificates.crt
logfile        /var/log/msmtp.log

account smtp2go
host %s
port %d
user %s
auth           login
password %s
from %s

account default : smtp2go
`
)

var (
	relayUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)
	relayHostPattern     = regexp.MustCompile(`^[A-Za-z0-9.:-]+$`)
	relayFromPattern     = regexp.MustCompile(`^[^\s@]+@[^\s@]+$`)
)

type mailRelayResource struct {
	data *providerData
}

type mailRelayModel struct {
	Node            types.String `tfsdk:"node"`
	VMID            types.Int64  `tfsdk:"vmid"`
	Kind            types.String `tfsdk:"kind"`
	Username        types.String `tfsdk:"username"`
	Description     types.String `tfsdk:"description"`
	Host            types.String `tfsdk:"host"`
	Port            types.Int64  `tfsdk:"port"`
	From            types.String `tfsdk:"from"`
	Path            types.String `tfsdk:"path"`
	PasswordVersion types.Int64  `tfsdk:"password_version"`
	SHA256          types.String `tfsdk:"sha256"`
	WriteID         types.String `tfsdk:"write_id"`
}

func newMailRelayResource() resource.Resource {
	return &mailRelayResource{}
}

func (r *mailRelayResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mail_relay"
}

func (r *mailRelayResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The resource declares one SMTP2GO SMTP user and an msmtp configuration file in one guest. " +
			"The provider generates the password and stores neither the password nor the API key in plans or state.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"username": schema.StringAttribute{
				Required: true,
				Description: "The username identifies the SMTP2GO SMTP user and sets the user line in the msmtp configuration file. " +
					"A username change replaces the resource.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(
						relayUsernamePattern,
						"must start with a letter or digit and contain only letters, digits, periods, underscores, @, or hyphens",
					),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"description": schema.StringAttribute{
				Optional: true,
				Description: "The provider sends the description to SMTP2GO only in the request to create a user. " +
					"A description change alone updates state without an API request.",
			},
			"host": schema.StringAttribute{
				Required: true,
				Description: "The relay host sets the host line in the msmtp configuration file. " +
					"A host change rewrites the file with a new password.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(relayHostPattern, "must contain only letters, digits, periods, colons, or hyphens"),
				},
			},
			"port": schema.Int64Attribute{
				Required:    true,
				Description: "The relay port must be between 1 and 65535. A port change rewrites the file with a new password.",
				Validators: []validator.Int64{
					int64validator.Between(minimumRelayPort, maximumRelayPort),
				},
			},
			"from": schema.StringAttribute{
				Required:    true,
				Description: "The from value sets the envelope sender. A change rewrites the file with a new password.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(
						relayFromPattern,
						"must contain exactly one @ with nonempty text on both sides and no whitespace",
					),
				},
			},
			"path": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultRelayFilePath),
				Description: "The absolute file path defaults to /etc/msmtprc. The file uses mode 0600 and owner root:root. " +
					"A path change replaces the resource.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(absolutePathPattern, "must be an absolute path that ends in a file name"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"password_version": schema.Int64Attribute{
				Required: true,
				Description: "A higher password_version generates a new password, updates the SMTP2GO user, and rewrites the file. " +
					"A lower value fails the plan.",
			},
			"sha256": schema.StringAttribute{
				Computed: true,
				Description: "The sha256 value is the hexadecimal SHA-256 hash of the guest file. " +
					"The provider plans a file rewrite with a new password when the guest hash differs from the last applied hash.",
			},
			"write_id": schema.StringAttribute{
				Computed:    true,
				Description: "The write_id value is a random identifier that changes each time an apply writes the file.",
			},
		}),
	}
}

func (r *mailRelayResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *mailRelayResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}
	var plan mailRelayModel
	var state mailRelayModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	versionsKnown := !plan.PasswordVersion.IsUnknown() && !state.PasswordVersion.IsNull()
	if versionsKnown && plan.PasswordVersion.ValueInt64() < state.PasswordVersion.ValueInt64() {
		resp.Diagnostics.AddAttributeError(
			fwpath.Root("password_version"),
			"password_version cannot decrease",
			fmt.Sprintf(
				"Planned password_version %d is lower than the state value %d. A higher password_version rotates the password.",
				plan.PasswordVersion.ValueInt64(), state.PasswordVersion.ValueInt64(),
			),
		)
		return
	}
	appliedHash, diagnostics := req.Private.GetKey(ctx, appliedHashEntry)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}

	// An unknown sha256 makes Update rotate the password and rewrite the file.
	inputsChanged := !plan.PasswordVersion.Equal(state.PasswordVersion) || !plan.Host.Equal(state.Host) ||
		!plan.Port.Equal(state.Port) || !plan.From.Equal(state.From)
	drifted := decodeAppliedHash(appliedHash) != state.SHA256.ValueString()
	if inputsChanged || drifted {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("sha256"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("write_id"), types.StringUnknown())...)
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("sha256"), state.SHA256)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("write_id"), state.WriteID)...)
}

func (r *mailRelayResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan mailRelayModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The next apply reruns Create after a failure without state.
	// SMTP2GO updates the password if the add request reports an existing user.
	if _, err := r.writeCredential(ctx, &plan); err != nil {
		resp.Diagnostics.AddError(writeRelaySummary, err.Error())
		return
	}
	appliedHash, err := encodeAppliedHash(plan.SHA256.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(writeRelaySummary, err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedHashEntry, appliedHash)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *mailRelayResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state mailRelayModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(state.Node, state.VMID, state.Kind)
	guestHash, found, err := readGuestSHA256(ctx, r.data.client, guest, state.Path.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(readRelaySummary, err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	state.SHA256 = types.StringValue(guestHash)

	username := state.Username.ValueString()
	user, userFound, err := r.data.smtp2go.readUser(ctx, username)
	switch {
	case err != nil:
		resp.Diagnostics.AddWarning(relayUserUnreadSummary, err.Error())
	case !userFound:
		resp.Diagnostics.AddWarning(relayUserMissingSummary, "SMTP2GO did not find user "+username)
		resp.State.RemoveResource(ctx)
		return
	case user.Status != smtp2goStatusAllowed:
		resp.Diagnostics.AddWarning(
			relayUserBlockedSummary,
			fmt.Sprintf("SMTP2GO user %s has status %s instead of allowed.", username, user.Status),
		)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *mailRelayResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan mailRelayModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Description changes require only a state update when sha256 remains known.
	if !plan.SHA256.IsUnknown() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	passwordRequested, err := r.writeCredential(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError(writeRelaySummary, err.Error())
		if !passwordRequested {
			return
		}
		// An empty applied hash forces a password update and file rewrite on the next apply,
		// even when the configuration matches state.
		clearedHash, encodeErr := encodeAppliedHash("")
		if encodeErr != nil {
			resp.Diagnostics.AddError(writeRelaySummary, encodeErr.Error())
			return
		}
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedHashEntry, clearedHash)...)
		return
	}
	appliedHash, err := encodeAppliedHash(plan.SHA256.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(writeRelaySummary, err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedHashEntry, appliedHash)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the guest file without deleting the SMTP2GO user.
func (r *mailRelayResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state mailRelayModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(state.Node, state.VMID, state.Kind)
	err := runChecked(ctx, r.data.client, guest, guestCommand("rm", "-f", "--", state.Path.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Delete failed to remove the guest file", err.Error())
	}
}

// writeCredential returns true after attempting the SMTP2GO password update, even on failure.
func (r *mailRelayResource) writeCredential(ctx context.Context, plan *mailRelayModel) (bool, error) {
	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	destination := plan.Path.ValueString()
	username := plan.Username.ValueString()
	password, err := newRelayPassword()
	if err != nil {
		return false, err
	}

	// A failed directory command stops the apply before the SMTP2GO password changes.
	directoryCommand := guestCommand("sh", "-c", `umask 022 && mkdir -p -- "$1"`, "sh", path.Dir(destination))
	if err := runChecked(ctx, r.data.client, guest, directoryCommand); err != nil {
		return false, err
	}

	err = r.data.smtp2go.setPassword(ctx, username, password, plan.Description.ValueString())
	if err != nil {
		return true, err
	}

	content := fmt.Appendf(
		nil, msmtprcFormat,
		plan.Host.ValueString(), plan.Port.ValueInt64(), username, password, plan.From.ValueString(),
	)
	files := &fileResource{data: r.data}
	fileAttributes := fileModel{
		Path:     plan.Path,
		Mode:     types.StringValue(relayFileMode),
		Owner:    types.StringValue(defaultFileOwner),
		Group:    types.StringValue(defaultFileGroup),
		Validate: types.StringNull(),
	}
	if err := files.writeFile(ctx, guest, fileAttributes, content); err != nil {
		return true, errors.New(redactSecret(err.Error(), password))
	}
	writeID, err := newWriteID()
	if err != nil {
		return true, err
	}
	plan.SHA256 = types.StringValue(hashHex(content))
	plan.WriteID = types.StringValue(writeID)
	return true, nil
}

func newRelayPassword() (string, error) {
	alphabetSize := big.NewInt(int64(len(relayPasswordAlphabet)))
	password := make([]byte, relayPasswordLength)
	for i := range password {
		index, err := rand.Int(rand.Reader, alphabetSize)
		if err != nil {
			slog.Error("Relay password generation failed", "err", err)
			return "", fmt.Errorf("generate mail relay password: %w", err)
		}
		password[i] = relayPasswordAlphabet[index.Int64()]
	}
	return string(password), nil
}
