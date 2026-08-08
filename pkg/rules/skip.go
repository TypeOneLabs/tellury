package rules

// SkipCode is a stable, machine-readable reason a resource was skipped by a
// rule. Distinct codes exist so `--explain-skips` can tell apart the many
// reasons a rule declines to emit a finding, and so a fresh rule can reuse the
// shared vocabulary instead of inventing literal string reasons.
type SkipCode string

// Shared skip codes. Rules MAY define package-local codes for reasons that are
// specific to their detection logic, and SHOULD reuse these shared ones
// whenever a reason matches an existing code rather than duplicating meaning.
const (
	// SkipExemptLabel: the resource carries the tellury-exempt=true label
	// (Invariant I7) and is intentionally out of scope for every rule.
	SkipExemptLabel SkipCode = "exempt_label"
	// SkipMissingAttr: the resource payload was ingested but a field the
	// rule needs is absent or unparsable; the rule refuses to guess.
	SkipMissingAttr SkipCode = "missing_attribute"
	// SkipBadAttrType: an attribute is present but not of the type the rule
	// expects (e.g. a number where a string discriminator is required).
	SkipBadAttrType SkipCode = "bad_attribute_type"
	// SkipAttached: the resource is in use — attached to an instance,
	// targeted by a forwarding rule, etc. Not waste.
	SkipAttached SkipCode = "in_use"
	// SkipNonBillingStatus: the resource status is one that incurs no stable
	// billable charge (e.g. a disk mid-deletion).
	SkipNonBillingStatus SkipCode = "non_billing_status"
	// SkipRecentlyDetached: present but only recently freed; below the
	// rule's age threshold and therefore not yet confidently waste.
	SkipRecentlyDetached SkipCode = "recently_detached"
	// SkipNoPrice: the SKU/region has no price in any source; the rule
	// refuses to assume $0 (Invariant I4).
	SkipNoPrice SkipCode = "no_price"
	// SkipBelowMinWaste: the computed waste is below the rule's noise
	// floor; reporting it would be noise, not signal.
	SkipBelowMinWaste SkipCode = "below_min_waste"
	// SkipUnknownMachineType: the machine-type token does not resolve in
	// the catalog (or the shape fields are absent), so no rightsizing
	// decision can be made.
	SkipUnknownMachineType SkipCode = "unknown_machine_type"
	// SkipNotRunning: the instance is not in RUNNING status and has no
	// steady-state compute minutes to reclaim.
	SkipNotRunning SkipCode = "not_running"
	// SkipSpot: the instance runs on a spot/preemptible model and already
	// pays a steeply discounted rate; no compute waste to reclaim.
	SkipSpot SkipCode = "spot"
	// SkipAccelerator: the instance carries GPUs, which the v1 rightsizing
	// model does not price.
	SkipAccelerator SkipCode = "accelerator_present"
	// SkipTooYoung: the instance is younger than the rule's minimum age
	// (bursty early-life CPU is not evidence of sustained underuse).
	SkipTooYoung SkipCode = "too_young"
	// SkipNoMetric: the rule is metric-dependent and the required series is
	// absent or below the minimum sample count (Invariant I5).
	SkipNoMetric SkipCode = "no_metric"
	// SkipLowCoverage: the metric series exists but covers too small a
	// fraction of the window to support a confident estimate.
	SkipLowCoverage SkipCode = "low_metric_coverage"
	// SkipBelowOverprovision: the measured utilization leaves the instance
	// below the rule's overprovision threshold — it is not overprovisioned.
	SkipBelowOverprovision SkipCode = "below_overprovision_threshold"
	// SkipNoSmallerSize: the rightsizing candidate ladder contains no
	// smaller shape that meets the utilization target for this family.
	SkipNoSmallerSize SkipCode = "no_smaller_size"
	// SkipHasLifecycle: the bucket already has at least one lifecycle rule.
	SkipHasLifecycle SkipCode = "has_lifecycle"
	// SkipNotStandardClass: the bucket is not STANDARD class, so a
	// NEARLINE class transition is not available.
	SkipNotStandardClass SkipCode = "not_standard_class"
	// SkipAutoclass: the bucket has Autoclass enabled, which obviates the
	// need for a manual lifecycle policy.
	SkipAutoclass SkipCode = "autoclass_enabled"
	// SkipRetentionLocked: the bucket's retention policy is locked, which
	// prevents any class transition.
	SkipRetentionLocked SkipCode = "retention_locked"
	// SkipBelowMinBytes: the stored bytes are below the rule's noise
	// floor; the class-delta saving would be noise.
	SkipBelowMinBytes SkipCode = "below_min_bytes"
	// SkipInternalAddress: the address is INTERNAL, and internal addresses
	// are free — not waste.
	SkipInternalAddress SkipCode = "internal_address"
	// SkipManagedByMIG: the instance belongs to a managed instance group,
	// which owns the member's size and count. GCP marks MIG members with the
	// `created-by` instance metadata item naming an instanceGroupManagers
	// resource; recommending a resize for one member is advice an operator
	// cannot act on, and the group's own sizing is a separate concern with
	// its own rules.
	SkipManagedByMIG SkipCode = "managed_by_mig"
)
