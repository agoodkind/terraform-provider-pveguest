package provider

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	redactedHeaderValue = "[redacted]"

	headerConfigLineFormat = "header = \"%s: %s\"\n"

	requestHeadersAttribute        = "request_headers_wo"
	requestHeadersVersionAttribute = "request_headers_wo_version"

	invalidHeaderNameSummary = "The HTTP request header name is invalid."
	invalidHeaderNameDetail  = "The HTTP request header name %q must contain only letters, digits, " +
		"and these characters: ! # $ %% & ' * + - . ^ _ ` | ~."
	invalidHeaderValueSummary = "The HTTP request header value is invalid."
	invalidHeaderValueDetail  = "The HTTP request header value for %q must not contain a carriage " +
		"return, a line feed, or a NUL character."
	unknownHeadersSummary = "request_headers_wo is unknown during apply."
	unknownHeadersDetail  = "Set request_headers_wo to a value that is known during apply."
)

var (
	headerNamePattern  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	headerValuePattern = regexp.MustCompile(`^[^\x00\r\n]*$`)

	curlConfigEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)
)

func (headers requestHeaders) sortedNames() []string {
	return slices.Sorted(maps.Keys(headers))
}

func (headers requestHeaders) curlConfig() []byte {
	var config strings.Builder

	for _, name := range headers.sortedNames() {
		fmt.Fprintf(&config, headerConfigLineFormat, name, curlConfigEscaper.Replace(headers[name]))
	}

	return []byte(config.String())
}

func (headers requestHeaders) redactText(text string) string {
	values := slices.Collect(maps.Values(headers))
	slices.SortFunc(
		values,
		func(left string, right string) int {
			return len(right) - len(left)
		},
	)

	for _, value := range values {
		if value == "" {
			continue
		}

		text = strings.ReplaceAll(text, value, redactedHeaderValue)
	}

	return text
}

func requestHeadersFrom(
	ctx context.Context,
	config tfsdk.Config,
) (requestHeaders, diag.Diagnostics) {
	var configured types.Map
	diagnostics := config.GetAttribute(ctx, fwpath.Root(requestHeadersAttribute), &configured)
	if diagnostics.HasError() {
		return nil, diagnostics
	}

	if configured.IsNull() {
		return nil, diagnostics
	}

	if configured.IsUnknown() {
		diagnostics.AddAttributeError(
			fwpath.Root(requestHeadersAttribute),
			unknownHeadersSummary,
			unknownHeadersDetail,
		)

		return nil, diagnostics
	}

	headers := requestHeaders{}
	diagnostics.Append(configured.ElementsAs(ctx, &headers, false)...)

	return headers, diagnostics
}

func validateRequestHeaders(ctx context.Context, configured types.Map) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	elements := map[string]types.String{}
	diagnostics.Append(configured.ElementsAs(ctx, &elements, false)...)
	if diagnostics.HasError() {
		return diagnostics
	}

	for _, name := range slices.Sorted(maps.Keys(elements)) {
		headerPath := fwpath.Root(requestHeadersAttribute).AtMapKey(name)
		if !headerNamePattern.MatchString(name) {
			diagnostics.AddAttributeError(
				headerPath,
				invalidHeaderNameSummary,
				fmt.Sprintf(invalidHeaderNameDetail, name),
			)
		}

		value := elements[name]
		if value.IsUnknown() || value.IsNull() {
			continue
		}

		if !headerValuePattern.MatchString(value.ValueString()) {
			diagnostics.AddAttributeError(
				headerPath,
				invalidHeaderValueSummary,
				fmt.Sprintf(invalidHeaderValueDetail, name),
			)
		}
	}

	return diagnostics
}

func withRequestHeaderAttributes(
	attributes map[string]schema.Attribute,
) map[string]schema.Attribute {
	attributes[requestHeadersAttribute] = schema.MapAttribute{
		ElementType: types.StringType,
		Optional:    true,
		Sensitive:   true,
		WriteOnly:   true,
		Description: "request_headers_wo is a sensitive, write-only map of HTTP request header names to values. " +
			"OpenTofu does not store its value in the plan or state. " +
			"When fetch = \"controller\", the provider sends the headers and removes them from redirected " +
			"requests that change the scheme, host, or port. " +
			"When fetch = \"guest\", curl sends the headers and does not follow redirects. " +
			"A cached object with the expected hash skips the request.",
		Validators: []validator.Map{
			mapvalidator.AlsoRequires(fwpath.MatchRoot(requestHeadersVersionAttribute)),
		},
	}
	attributes[requestHeadersVersionAttribute] = schema.Int64Attribute{
		Optional: true,
		Description: "The provider requires request_headers_wo_version when request_headers_wo is set. " +
			"Changing request_headers_wo_version plans a rewrite of the resource. " +
			"During apply, the provider fetches the object using the headers from the current configuration. " +
			"With fetch = \"controller\", the controller reuses a cached object that matches the configured sha256 " +
			"without sending an HTTP request or using the headers. " +
			"With fetch = \"guest\", the provider always runs curl in the guest with the headers.",
		Validators: []validator.Int64{
			int64validator.AlsoRequires(fwpath.MatchRoot(requestHeadersAttribute)),
		},
	}

	return attributes
}
