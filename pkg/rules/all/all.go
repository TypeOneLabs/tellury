// Package all registers every built-in rule via import side effects. This is the
// ONLY place rule packages are referenced, keeping pkg/rules free of any
// dependency on concrete rules.
package all

import (
	_ "github.com/TypeOneLabs/tellury/pkg/rules/aws/ec2/unassociated_eip"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/aws/ec2/unattached_ebs_volume"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/aws/ec2/underutilized_instance"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/azure/compute/unattached_managed_disk"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/azure/network/unassociated_public_ip"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/detached_disk"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/old_snapshot"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/underutilized_instance"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/unused_reserved_ip"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/gcs/no_lifecycle_policy"
)
