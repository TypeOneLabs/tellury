package gcp

import (
	"context"
	"log/slog"

	billing "cloud.google.com/go/billing/apiv1"
	billingpb "cloud.google.com/go/billing/apiv1/billingpb"
)

// DetectCurrency is the best-effort auto-detection of the currency a scan's
// prices should be in. The source of truth is the billing account the scanned
// project is attached to: a billing account's currency is fixed at creation,
// so the two-hop path
//
//	GetProjectBillingInfo("projects/<project>") -> billing account name
//	GetBillingAccount(name)                      -> CurrencyCode
//
// is a reliable answer when the caller has billing permission. Both hops can
// fail independently with PermissionDenied, and against a real organization
// they do: the first hop succeeded for one project and returned
// "billingAccounts/011717-4B0B6E-0A2711" while the second was denied, and a
// second project was denied at the first hop. ListBillingAccounts is NOT
// used for detection — it returns an empty list (not an error) without
// permission, which is indistinguishable from "no accounts".
//
// Detection is therefore deliberately best effort: it asks each project in
// turn and stops at the first success, so a folder/organization scan can use
// any project in scope that answers. Returns ("", "") when no project
// answered — every hop was denied, the project had no billing account, or no
// Cloud Billing client could be built. The caller falls back to USD quietly:
// a missing billing role is a normal state for an otherwise-healthy scan.
// Every refusal is logged at debug.
func DetectCurrency(ctx context.Context, log *slog.Logger, projects []string) (code, project string) {
	if log == nil {
		log = slog.Default()
	}
	if len(projects) == 0 {
		log.Debug("gcp: currency detection skipped: no project in scope to ask")
		return "", ""
	}
	client, err := billing.NewCloudBillingClient(ctx)
	if err != nil {
		log.Debug("gcp: currency detection unavailable (no Cloud Billing client); defaulting to USD", "err", err)
		return "", ""
	}
	defer func() { _ = client.Close() }()

	for _, project := range projects {
		info, err := client.GetProjectBillingInfo(ctx, &billingpb.GetProjectBillingInfoRequest{
			Name: "projects/" + project,
		})
		if err != nil {
			log.Debug("gcp: currency detection: GetProjectBillingInfo failed; trying next project",
				"project", project, "err", mapBillingError("GetProjectBillingInfo", err))
			continue
		}
		acctName := info.GetBillingAccountName()
		if acctName == "" {
			log.Debug("gcp: currency detection: project has no billing account; trying next project",
				"project", project)
			continue
		}
		acct, err := client.GetBillingAccount(ctx, &billingpb.GetBillingAccountRequest{Name: acctName})
		if err != nil {
			log.Debug("gcp: currency detection: GetBillingAccount failed; trying next project",
				"project", project, "billing_account", acctName, "err", mapBillingError("GetBillingAccount", err))
			continue
		}
		code := acct.GetCurrencyCode()
		if code == "" {
			log.Debug("gcp: currency detection: billing account carries no currency code; trying next project",
				"project", project, "billing_account", acctName)
			continue
		}
		log.Debug("gcp: currency detected from billing account",
			"currency", code, "project", project, "billing_account", acctName)
		return code, project
	}
	log.Debug("gcp: currency detection: no project in scope answered; defaulting to USD", "projects", len(projects))
	return "", ""
}
