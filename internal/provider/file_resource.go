package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const (
	defaultFileMode  = "0644"
	defaultFileOwner = "root"
	defaultFileGroup = "root"

	modeDigits              = 4
	sha256HexLength         = sha256.Size * 2
	temporarySuffixBytes    = 6
	writeIDBytes            = 8
	validatePathPlaceholder = "%s"
	missingFileMessage      = "No such file or directory"

	// The private state key stores the hash that the last apply wrote. Read
	// overwrites the sha256 attribute with the guest hash, and the plan
	// compares both to detect drift of write-only content.
	appliedHashEntry = "applied_sha256"
)

var (
	absolutePathPattern = regexp.MustCompile(`^/[^\x00]*[^/\x00]$`)
	fileModePattern     = regexp.MustCompile(`^[0-7]{4}$`)
	accountNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\$?$`)
	sha256Pattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type fileResource struct {
	data *providerData
}

type fileModel struct {
	Node             types.String `tfsdk:"node"`
	VMID             types.Int64  `tfsdk:"vmid"`
	Kind             types.String `tfsdk:"kind"`
	Path             types.String `tfsdk:"path"`
	Content          types.String `tfsdk:"content"`
	ContentWO        types.String `tfsdk:"content_wo"`
	ContentWOVersion types.Int64  `tfsdk:"content_wo_version"`
	Mode             types.String `tfsdk:"mode"`
	Owner            types.String `tfsdk:"owner"`
	Group            types.String `tfsdk:"group"`
	Validate         types.String `tfsdk:"validate"`
	SHA256           types.String `tfsdk:"sha256"`
	WriteID          types.String `tfsdk:"write_id"`
}

type fileStatus struct {
	Mode   string
	Owner  string
	Group  string
	SHA256 string
}

func newFileResource() resource.Resource {
	return &fileResource{}
}

func (r *fileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file"
}

func (r *fileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A regular file inside a guest.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required:    true,
				Description: "Absolute path of the file. An apply creates missing parent directories with mode 0755 and owner root:root.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(absolutePathPattern, "must be an absolute path that ends in a file name"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"content": schema.StringAttribute{
				Optional:    true,
				Description: "File content. Conflicts with content_wo.",
				Validators: []validator.String{
					stringvalidator.ExactlyOneOf(fwpath.MatchRoot("content_wo")),
				},
			},
			"content_wo": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				WriteOnly:   true,
				Description: "Specify file content that OpenTofu does not store in plans or state. Also set content_wo_version.",
				Validators: []validator.String{
					stringvalidator.AlsoRequires(fwpath.MatchRoot("content_wo_version")),
				},
			},
			"content_wo_version": schema.Int64Attribute{
				Optional:    true,
				Description: "A changed value writes content_wo to the guest again.",
				Validators: []validator.Int64{
					int64validator.AlsoRequires(fwpath.MatchRoot("content_wo")),
				},
			},
			"mode": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(defaultFileMode),
				Description: "Permission bits as four octal digits. The default is 0644.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(fileModePattern, "must be four octal digits, for example 0644"),
				},
			},
			"owner": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(defaultFileOwner),
				Description: "Owner user name. The default is root.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(accountNamePattern, "must be a user name"),
				},
			},
			"group": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(defaultFileGroup),
				Description: "Owner group name. The default is root.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(accountNamePattern, "must be a group name"),
				},
			},
			"validate": schema.StringAttribute{
				Optional: true,
				Description: "Shell command that runs in the guest on the temporary file before the rename. " +
					"The shell-quoted path of the temporary file replaces %s. A nonzero exit status fails the apply.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(regexp.MustCompile(validatePathPlaceholder), "must contain %s"),
				},
			},
			"sha256": schema.StringAttribute{
				Computed:    true,
				Description: "SHA-256 hash of the file content in the guest, in hexadecimal.",
			},
			"write_id": schema.StringAttribute{
				Computed: true,
				Description: "Random identifier that changes each time an apply writes the content, mode, owner, or group. " +
					"A rewrite after drift produces the same sha256 and a new write_id.",
			},
		}),
	}
}

func (r *fileResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *fileResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan fileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state fileModel
	hasState := !req.State.Raw.IsNull()
	if hasState {
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	plannedHash := types.StringUnknown()
	if !plan.Content.IsNull() {
		if !plan.Content.IsUnknown() {
			plannedHash = types.StringValue(hashHex([]byte(plan.Content.ValueString())))
		}
	} else if hasState {
		// OpenTofu omits write-only content from the plan. The hash is unknown
		// until apply whenever the file must be written.
		appliedHash, diagnostics := req.Private.GetKey(ctx, appliedHashEntry)
		resp.Diagnostics.Append(diagnostics...)
		if resp.Diagnostics.HasError() {
			return
		}
		sameSource := state.Content.IsNull() && state.ContentWOVersion.Equal(plan.ContentWOVersion)
		guestMatchesApply := decodeAppliedHash(appliedHash) == state.SHA256.ValueString()
		if sameSource && guestMatchesApply {
			plannedHash = state.SHA256
		}
	}
	plan.SHA256 = plannedHash

	plannedWriteID := types.StringUnknown()
	if hasState && !guestWritePlanned(plan, state) {
		plannedWriteID = state.WriteID
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("sha256"), plannedHash)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("write_id"), plannedWriteID)...)
}

// Updating validate alone does not rewrite the file.
func guestWritePlanned(plan fileModel, state fileModel) bool {
	sameContent := !plan.SHA256.IsUnknown() && plan.SHA256.Equal(state.SHA256)
	sameAttributes := plan.Mode.Equal(state.Mode) && plan.Owner.Equal(state.Owner) && plan.Group.Equal(state.Group)
	return !sameContent || !sameAttributes
}

func (r *fileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	content, diagnostics := fileContent(ctx, plan, req.Config)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	if err := r.writeFile(ctx, guest, plan, content); err != nil {
		resp.Diagnostics.AddError("Write file", err.Error())
		return
	}

	plan.SHA256 = types.StringValue(hashHex(content))
	writeID, err := newWriteID()
	if err != nil {
		resp.Diagnostics.AddError("Write file", err.Error())
		return
	}
	plan.WriteID = types.StringValue(writeID)
	appliedHash, err := encodeAppliedHash(plan.SHA256.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Store applied hash", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedHashEntry, appliedHash)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *fileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state fileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	status, found, err := readFileStatus(ctx, r.data.client, guest, state.Path.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Read file", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	state.Mode = types.StringValue(status.Mode)
	state.Owner = types.StringValue(status.Owner)
	state.Group = types.StringValue(status.Group)
	state.SHA256 = types.StringValue(status.SHA256)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *fileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan fileModel
	var state fileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	contentIsCurrent := !plan.SHA256.IsUnknown() && plan.SHA256.Equal(state.SHA256)
	switch {
	case !contentIsCurrent:
		content, diagnostics := fileContent(ctx, plan, req.Config)
		resp.Diagnostics.Append(diagnostics...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := r.writeFile(ctx, guest, plan, content); err != nil {
			resp.Diagnostics.AddError("Write file", err.Error())
			return
		}
		plan.SHA256 = types.StringValue(hashHex(content))
	case guestWritePlanned(plan, state):
		// Only mode, owner, or group differ. The content in the guest
		// already has the planned hash.
		if err := r.setAttributes(ctx, guest, plan, plan.Path.ValueString()); err != nil {
			resp.Diagnostics.AddError("Update file attributes", err.Error())
			return
		}
	}

	if plan.WriteID.IsUnknown() {
		writeID, err := newWriteID()
		if err != nil {
			resp.Diagnostics.AddError("Write file", err.Error())
			return
		}
		plan.WriteID = types.StringValue(writeID)
	}
	appliedHash, err := encodeAppliedHash(plan.SHA256.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Store applied hash", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedHashEntry, appliedHash)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *fileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state fileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(state.Node, state.VMID, state.Kind)
	err := runChecked(ctx, r.data.client, guest, guestCommand("rm", "-f", "--", state.Path.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Delete file", err.Error())
	}
}

// The provider reads write-only content from configuration during apply.
func fileContent(ctx context.Context, plan fileModel, config tfsdk.Config) ([]byte, diag.Diagnostics) {
	if !plan.Content.IsNull() {
		return []byte(plan.Content.ValueString()), nil
	}
	var writeOnlyContent types.String
	diagnostics := config.GetAttribute(ctx, fwpath.Root("content_wo"), &writeOnlyContent)
	if diagnostics.HasError() {
		return nil, diagnostics
	}
	if writeOnlyContent.IsNull() || writeOnlyContent.IsUnknown() {
		diagnostics.AddError("Missing file content", "The apply needs a value for content or content_wo.")
		return nil, diagnostics
	}
	return []byte(writeOnlyContent.ValueString()), diagnostics
}

// writeFile writes the content to a temporary file in the directory of the
// destination. A rename inside one directory is atomic.
func (r *fileResource) writeFile(ctx context.Context, guest transport.Guest, plan fileModel, content []byte) error {
	destination := plan.Path.ValueString()
	temporaryPath, err := temporaryPathFor(destination)
	if err != nil {
		return err
	}

	// mkdir -m sets the mode of the last directory only. The umask gives
	// every new directory in the path mode 0755. The command runs as root,
	// and mkdir -p applies the umask only to the directories that it creates.
	directoryCommand := guestCommand("sh", "-c", `umask 022 && mkdir -p -- "$1"`, "sh", path.Dir(destination))
	if err := runChecked(ctx, r.data.client, guest, directoryCommand); err != nil {
		return err
	}

	if err := r.writeContent(ctx, guest, temporaryPath, content); err != nil {
		r.removeTemporaryFile(ctx, guest, temporaryPath)
		return err
	}
	if err := r.setAttributes(ctx, guest, plan, temporaryPath); err != nil {
		r.removeTemporaryFile(ctx, guest, temporaryPath)
		return err
	}
	if !plan.Validate.IsNull() {
		validateScript := validateCommandLine(plan.Validate.ValueString(), temporaryPath)
		result, err := runGuest(ctx, r.data.client, guest, guestCommand("sh", "-c", validateScript))
		if err != nil {
			r.removeTemporaryFile(ctx, guest, temporaryPath)
			return err
		}
		if result.ExitCode != 0 {
			r.removeTemporaryFile(ctx, guest, temporaryPath)
			return fmt.Errorf(
				"%s: validate command for %s exited with status %d. Output: %q",
				guest, destination, result.ExitCode, combinedOutput(result),
			)
		}
	}
	// -T makes mv fail on a directory at the destination instead of moving
	// the temporary file into it.
	moveCommand := guestCommand("mv", "-fT", "--", temporaryPath, destination)
	if err := runChecked(ctx, r.data.client, guest, moveCommand); err != nil {
		r.removeTemporaryFile(ctx, guest, temporaryPath)
		return err
	}
	return nil
}

// One command accepts a limited standard input. Larger content takes one
// command that truncates the temporary file and one command per piece that
// appends. The umask makes the temporary file unreadable for other users until
// chmod sets the declared mode.
func (r *fileResource) writeContent(
	ctx context.Context,
	guest transport.Guest,
	temporaryPath string,
	content []byte,
) error {
	if len(content) <= guest.Kind.StdinLimit() {
		writeCommand := guestCommand("sh", "-c", `umask 077 && cat > "$1"`, "sh", temporaryPath)
		writeCommand.Stdin = content
		return runChecked(ctx, r.data.client, guest, writeCommand)
	}

	truncateCommand := guestCommand("sh", "-c", `umask 077 && : > "$1"`, "sh", temporaryPath)
	if err := runChecked(ctx, r.data.client, guest, truncateCommand); err != nil {
		return err
	}
	for _, piece := range transport.SplitStdin(guest.Kind, content) {
		appendCommand := guestCommand("sh", "-c", `cat >> "$1"`, "sh", temporaryPath)
		appendCommand.Stdin = piece
		if err := runChecked(ctx, r.data.client, guest, appendCommand); err != nil {
			return err
		}
	}
	return nil
}

func (r *fileResource) setAttributes(ctx context.Context, guest transport.Guest, plan fileModel, targetPath string) error {
	return setGuestAttributes(ctx, r.data.client, guest, plan.Mode, plan.Owner, plan.Group, targetPath)
}

func setGuestAttributes(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	mode types.String,
	owner types.String,
	group types.String,
	targetPath string,
) error {
	modeCommand := guestCommand("chmod", mode.ValueString(), "--", targetPath)
	if err := runChecked(ctx, client, guest, modeCommand); err != nil {
		return err
	}
	ownership := owner.ValueString() + ":" + group.ValueString()
	return runChecked(ctx, client, guest, guestCommand("chown", ownership, "--", targetPath))
}

// removeTemporaryFile runs after another step failed. The caller reports
// that failure and ignores a failed removal.
func (r *fileResource) removeTemporaryFile(ctx context.Context, guest transport.Guest, temporaryPath string) {
	_, _ = r.data.client.Run(ctx, guest, guestCommand("rm", "-f", "--", temporaryPath))
}

func readFileStatus(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	filePath string,
) (fileStatus, bool, error) {
	statCommand := guestCommand("stat", "-L", "-c", "%a %U %G", "--", filePath)
	statResult, err := runGuest(ctx, client, guest, statCommand)
	if err != nil {
		return fileStatus{}, false, err
	}
	if statResult.ExitCode != 0 {
		if strings.Contains(string(statResult.Stderr), missingFileMessage) {
			return fileStatus{}, false, nil
		}
		return fileStatus{}, false, commandFailure(guest, statCommand, statResult)
	}
	status, err := parseStatOutput(firstLine(statResult.Stdout))
	if err != nil {
		slog.ErrorContext(
			ctx, "stat output of a guest file is malformed",
			"node", guest.Node, "vmid", guest.VMID, "kind", guest.Kind, "err", err,
		)
		return fileStatus{}, false, fmt.Errorf("%s: parse stat output: %w", guest, err)
	}

	// The file can disappear between stat and sha256sum.
	hash, found, err := readGuestSHA256(ctx, client, guest, filePath)
	if err != nil || !found {
		return fileStatus{}, false, err
	}
	status.SHA256 = hash
	return status, true, nil
}

func readGuestSHA256(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	filePath string,
) (string, bool, error) {
	hashCommand := guestCommand("sha256sum", "--", filePath)
	hashResult, err := runGuest(ctx, client, guest, hashCommand)
	if err != nil {
		return "", false, err
	}
	if hashResult.ExitCode != 0 {
		if strings.Contains(string(hashResult.Stderr), missingFileMessage) {
			return "", false, nil
		}
		return "", false, commandFailure(guest, hashCommand, hashResult)
	}
	hash, err := parseSHA256SumOutput(firstLine(hashResult.Stdout))
	if err != nil {
		slog.ErrorContext(
			ctx, "sha256sum output of a guest file is malformed",
			"node", guest.Node, "vmid", guest.VMID, "kind", guest.Kind, "err", err,
		)
		return "", false, fmt.Errorf("%s: parse sha256sum output: %w", guest, err)
	}
	return hash, true, nil
}

func parseStatOutput(line string) (fileStatus, error) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return fileStatus{}, fmt.Errorf("unexpected stat output %q", line)
	}
	mode := fields[0]
	if len(mode) < modeDigits {
		mode = strings.Repeat("0", modeDigits-len(mode)) + mode
	}
	return fileStatus{Mode: mode, Owner: fields[1], Group: fields[2]}, nil
}

func parseSHA256SumOutput(line string) (string, error) {
	// sha256sum starts the line with a backslash when it escapes
	// characters in the file name.
	line = strings.TrimPrefix(line, `\`)
	if len(line) < sha256HexLength {
		return "", fmt.Errorf("unexpected sha256sum output %q", line)
	}
	hash := line[:sha256HexLength]
	if !sha256Pattern.MatchString(hash) {
		return "", fmt.Errorf("unexpected sha256sum output %q", line)
	}
	return hash, nil
}

func temporaryPathFor(destination string) (string, error) {
	suffix := make([]byte, temporarySuffixBytes)
	if _, err := rand.Read(suffix); err != nil {
		slog.Error("generate temporary file name failed", "err", err)
		return "", fmt.Errorf("generate temporary file name: %w", err)
	}
	name := "." + path.Base(destination) + ".pveguest-" + hex.EncodeToString(suffix)
	return path.Join(path.Dir(destination), name), nil
}

// The framework accepts only JSON documents as private state values.
type appliedHashDocument struct {
	SHA256 string `json:"sha256"`
}

func encodeAppliedHash(hash string) ([]byte, error) {
	document, err := json.Marshal(appliedHashDocument{SHA256: hash})
	if err != nil {
		slog.Error("encode applied hash failed", "err", err)
		return nil, fmt.Errorf("encode applied hash: %w", err)
	}
	return document, nil
}

// decodeAppliedHash returns an empty string for a missing or unreadable
// document. The plan then writes the file.
func decodeAppliedHash(document []byte) string {
	var decoded appliedHashDocument
	if err := json.Unmarshal(document, &decoded); err != nil {
		return ""
	}
	return decoded.SHA256
}

func newWriteID() (string, error) {
	identifier := make([]byte, writeIDBytes)
	if _, err := rand.Read(identifier); err != nil {
		slog.Error("generate write_id failed", "err", err)
		return "", fmt.Errorf("generate write_id: %w", err)
	}
	return hex.EncodeToString(identifier), nil
}

func validateCommandLine(template string, temporaryPath string) string {
	return strings.ReplaceAll(template, validatePathPlaceholder, transport.Quote(temporaryPath))
}

func hashHex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
