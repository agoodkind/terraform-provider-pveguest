package provider

import (
	"context"
	"math"

	"github.com/agoodkind/terraform-provider-pveguest/internal/netconfig"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

const minimumBondMembers = 2

func bondAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional:    true,
		Description: "Configures an active-backup bond. Each member retains its assigned Ethernet MAC.",
		Attributes: map[string]schema.Attribute{
			"members": schema.ListNestedAttribute{
				Required:   true,
				Validators: []validator.List{listvalidator.SizeAtLeast(minimumBondMembers)},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						networkNameField: schema.StringAttribute{
							Required: true,
							Validators: []validator.String{
								stringvalidator.RegexMatches(
									linuxInterfaceName,
									"must be a Linux interface name",
								),
							},
						},
						"mac": schema.StringAttribute{Required: true},
					},
				},
			},
			"primary": schema.StringAttribute{Required: true},
			"miimon": schema.Int64Attribute{
				Required:    true,
				Description: "Link monitoring interval in milliseconds.",
				Validators:  []validator.Int64{int64validator.Between(1, math.MaxInt32)},
			},
			"fail_over_mac": schema.StringAttribute{
				Required:   true,
				Validators: []validator.String{stringvalidator.OneOf("active")},
			},
			"primary_reselect": schema.StringAttribute{
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf("always", "better", "failure"),
				},
			},
		},
	}
}

func (m guestNetworkModel) bondSettings(
	ctx context.Context,
	diags *diag.Diagnostics,
) *netconfig.Bond {
	if m.Bond.IsNull() {
		return nil
	}

	value, err := m.Bond.ToTerraformValue(ctx)
	if err != nil {
		diags.AddAttributeError(path.Root("bond"), "Invalid guest bond", err.Error())

		return nil
	}

	if !value.IsFullyKnown() {
		return nil
	}

	var model struct {
		Members []struct {
			Name string `tfsdk:"name"`
			MAC  string `tfsdk:"mac"`
		} `tfsdk:"members"`
		Primary         string `tfsdk:"primary"`
		MIIMon          int64  `tfsdk:"miimon"`
		FailOverMAC     string `tfsdk:"fail_over_mac"`
		PrimaryReselect string `tfsdk:"primary_reselect"`
	}
	diags.Append(m.Bond.As(ctx, &model, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return nil
	}

	bond := netconfig.Bond{
		Primary:         model.Primary,
		MIIMon:          model.MIIMon,
		FailOverMAC:     model.FailOverMAC,
		PrimaryReselect: model.PrimaryReselect,
	}
	for _, member := range model.Members {
		bond.Members = append(
			bond.Members,
			netconfig.BondMember{Name: member.Name, MAC: member.MAC},
		)
	}

	if err := bond.Validate(m.Name.ValueString()); err != nil {
		diags.AddAttributeError(path.Root("bond"), "Invalid guest bond", err.Error())
	}

	return &bond
}
