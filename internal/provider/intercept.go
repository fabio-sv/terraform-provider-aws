package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-provider-aws/internal/conns"
	"github.com/hashicorp/terraform-provider-aws/internal/errs/sdkdiag"
	"github.com/hashicorp/terraform-provider-aws/internal/slices"
	tftags "github.com/hashicorp/terraform-provider-aws/internal/tags"
	"github.com/hashicorp/terraform-provider-aws/internal/types"
	"github.com/hashicorp/terraform-provider-aws/internal/verify"
	"github.com/hashicorp/terraform-provider-aws/names"
)

// An interceptor is functionality invoked during the CRUD request lifecycle.
// If a Before interceptor returns Diagnostics indicating an error occurred then
// no further interceptors in the chain are run and neither is the schema's method.
// In other cases all interceptors in the chain are run.
type interceptor interface {
	run(context.Context, *schema.ResourceData, any, When, Why, diag.Diagnostics) (context.Context, diag.Diagnostics)
}

type interceptorFunc func(context.Context, *schema.ResourceData, any, When, Why, diag.Diagnostics) (context.Context, diag.Diagnostics)

func (f interceptorFunc) run(ctx context.Context, d *schema.ResourceData, meta any, when When, why Why, diags diag.Diagnostics) (context.Context, diag.Diagnostics) {
	return f(ctx, d, meta, when, why, diags)
}

// interceptorItem represents a single interceptor invocation.
type interceptorItem struct {
	When        When
	Why         Why
	Interceptor interceptor
}

// When represents the point in the CRUD request lifecycle that an interceptor is run.
// Multiple values can be ORed together.
type When uint16

const (
	Before  When = 1 << iota // Interceptor is invoked before call to method in schema
	After                    // Interceptor is invoked after successful call to method in schema
	OnError                  // Interceptor is invoked after unsuccessful call to method in schema
	Finally                  // Interceptor is invoked after After or OnError
)

// Why represents the CRUD operation(s) that an interceptor is run.
// Multiple values can be ORed together.
type Why uint16

const (
	Create Why = 1 << iota // Interceptor is invoked for a Create call
	Read                   // Interceptor is invoked for a Read call
	Update                 // Interceptor is invoked for a Update call
	Delete                 // Interceptor is invoked for a Delete call

	AllOps = Create | Read | Update | Delete // Interceptor is invoked for all calls
)

type interceptorItems []interceptorItem

// Why returns a slice of interceptors that run for the specified CRUD operation.
func (s interceptorItems) Why(why Why) interceptorItems {
	return slices.Filter(s, func(t interceptorItem) bool {
		return t.Why&why != 0
	})
}

// interceptedHandler returns a handler that invokes the specified CRUD handler, running any interceptors.
func interceptedHandler[F ~func(context.Context, *schema.ResourceData, any) diag.Diagnostics](bootstrapContext contextFunc, interceptors interceptorItems, f F, why Why) F {
	return func(ctx context.Context, d *schema.ResourceData, meta any) diag.Diagnostics {
		var diags diag.Diagnostics
		ctx = bootstrapContext(ctx, meta)
		forward := interceptors.Why(why)

		when := Before
		for _, v := range forward {
			if v.When&when != 0 {
				ctx, diags = v.Interceptor.run(ctx, d, meta, when, why, diags)

				// Short circuit if any Before interceptor errors.
				if diags.HasError() {
					return diags
				}
			}
		}

		reverse := slices.Reverse(forward)
		diags = f(ctx, d, meta)

		if diags.HasError() {
			when = OnError
			for _, v := range reverse {
				if v.When&when != 0 {
					ctx, diags = v.Interceptor.run(ctx, d, meta, when, why, diags)
				}
			}
		} else {
			when = After
			for _, v := range reverse {
				if v.When&when != 0 {
					ctx, diags = v.Interceptor.run(ctx, d, meta, when, why, diags)
				}
			}
		}

		for _, v := range reverse {
			when = Finally
			if v.When&when != 0 {
				ctx, diags = v.Interceptor.run(ctx, d, meta, when, why, diags)
			}
		}

		return diags
	}
}

type contextFunc func(context.Context, any) context.Context

// dataSource represents an interceptor dispatcher for a Plugin SDK v2 data source.
type dataSource struct {
	bootstrapContext contextFunc
	interceptors     interceptorItems
}

func (ds *dataSource) Read(f schema.ReadContextFunc) schema.ReadContextFunc {
	return interceptedHandler(ds.bootstrapContext, ds.interceptors, f, Read)
}

// resource represents an interceptor dispatcher for a Plugin SDK v2 resource.
type resource struct {
	bootstrapContext contextFunc
	interceptors     interceptorItems
}

func (r *resource) Create(f schema.CreateContextFunc) schema.CreateContextFunc {
	return interceptedHandler(r.bootstrapContext, r.interceptors, f, Create)
}

func (r *resource) Read(f schema.ReadContextFunc) schema.ReadContextFunc {
	return interceptedHandler(r.bootstrapContext, r.interceptors, f, Read)
}

func (r *resource) Update(f schema.UpdateContextFunc) schema.UpdateContextFunc {
	return interceptedHandler(r.bootstrapContext, r.interceptors, f, Update)
}

func (r *resource) Delete(f schema.DeleteContextFunc) schema.DeleteContextFunc {
	return interceptedHandler(r.bootstrapContext, r.interceptors, f, Delete)
}

func (r *resource) State(f schema.StateContextFunc) schema.StateContextFunc {
	return func(ctx context.Context, d *schema.ResourceData, meta any) ([]*schema.ResourceData, error) {
		ctx = r.bootstrapContext(ctx, meta)

		return f(ctx, d, meta)
	}
}

func (r *resource) CustomizeDiff(f schema.CustomizeDiffFunc) schema.CustomizeDiffFunc {
	return func(ctx context.Context, d *schema.ResourceDiff, meta any) error {
		ctx = r.bootstrapContext(ctx, meta)

		return f(ctx, d, meta)
	}
}

func (r *resource) StateUpgrade(f schema.StateUpgradeFunc) schema.StateUpgradeFunc {
	return func(ctx context.Context, rawState map[string]interface{}, meta any) (map[string]interface{}, error) {
		ctx = r.bootstrapContext(ctx, meta)

		return f(ctx, rawState, meta)
	}
}

type tagsInterceptor struct {
	tags *types.ServicePackageResourceTags
}

func (r tagsInterceptor) run(ctx context.Context, d *schema.ResourceData, meta any, when When, why Why, diags diag.Diagnostics) (context.Context, diag.Diagnostics) {
	if r.tags == nil {
		return ctx, diags
	}

	inContext, ok := conns.FromContext(ctx)

	if !ok {
		return ctx, diags
	}

	sp, ok := meta.(*conns.AWSClient).ServicePackages[inContext.ServicePackageName]

	if !ok {
		return ctx, diags
	}

	serviceName, err := names.HumanFriendly(inContext.ServicePackageName)

	if err != nil {
		serviceName = "<service>"
	}

	resourceName := inContext.ResourceName

	if resourceName == "" {
		resourceName = "<thing>"
	}

	t, ok := tftags.FromContext(ctx)
	if !ok {
		return ctx, diags
	}

	switch when {
	case Before:
		switch why {
		case Create:
			tags := t.DefaultConfig.MergeTags(tftags.New(ctx, d.Get("tags").(map[string]interface{})))
			tags = tags.IgnoreAWS()

			t.TagsIn = tags
		case Update:
			if v, ok := sp.(interface {
				UpdateTags(context.Context, any, string, any, any) error
			}); ok {
				var identifier string

				if key := r.tags.IdentifierAttribute; key == "id" {
					identifier = d.Id()
				} else {
					identifier = d.Get(key).(string)
				}

				if d.HasChange("tags_all") {
					o, n := d.GetChange("tags_all")
					err := v.UpdateTags(ctx, meta, identifier, o, n)

					if verify.ErrorISOUnsupported(meta.(*conns.AWSClient).Partition, err) {
						// ISO partitions may not support tagging, giving error
						tflog.Warn(ctx, "failed updating tags for resource", map[string]interface{}{
							r.tags.IdentifierAttribute: d.Id(),
							"error":                    err.Error(),
						})
						return ctx, diags
					}

					if err != nil {
						return ctx, sdkdiag.AppendErrorf(diags, "updating tags for %s %s (%s): %s", serviceName, resourceName, identifier, err)
					}
				}
			}
		}
	case After:
		switch why {
		case Read:
			// may occur on a refresh when the resource does not exist in AWS and needs to be recreated
			// Disappears test
			if d.Id() == "" {
				return ctx, diags
			}

			fallthrough
		case Create, Update:
			if t.TagsOut.IsNone() {
				if v, ok := sp.(interface {
					ListTags(context.Context, any, string) (tftags.KeyValueTags, error)
				}); ok {
					var identifier string

					if key := r.tags.IdentifierAttribute; key == "id" {
						identifier = d.Id()
					} else {
						identifier = d.Get(key).(string)
					}

					tags, err := v.ListTags(ctx, meta, identifier)

					if verify.ErrorISOUnsupported(meta.(*conns.AWSClient).Partition, err) {
						// ISO partitions may not support tagging, giving error
						tflog.Warn(ctx, "failed listing tags for resource", map[string]interface{}{
							r.tags.IdentifierAttribute: d.Id(),
							"error":                    err.Error(),
						})
						return ctx, diags
					}

					if err != nil {
						return ctx, sdkdiag.AppendErrorf(diags, "listing tags for %s %s (%s): %s", serviceName, resourceName, identifier, err)
					}

					t.TagsOut = types.Some(tags)
				}
			}

			tags := t.TagsOut.UnwrapOrDefault().IgnoreAWS().IgnoreConfig(t.IgnoreConfig)

			if err := d.Set("tags", tags.RemoveDefaultConfig(t.DefaultConfig).Map()); err != nil {
				return ctx, sdkdiag.AppendErrorf(diags, "setting tags: %s", err)
			}

			if err := d.Set("tags_all", tags.Map()); err != nil {
				return ctx, sdkdiag.AppendErrorf(diags, "setting tags_all: %s", err)
			}
		}
	}

	return ctx, diags
}
