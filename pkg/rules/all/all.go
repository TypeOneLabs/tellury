// Package all registers every built-in rule via import side effects. This is
// the ONLY place rule packages are referenced, keeping pkg/rules free of any
// dependency on concrete rules.
package all

import (
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/detached_disk"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/underutilized_instance"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/compute/unused_reserved_ip"
	_ "github.com/TypeOneLabs/tellury/pkg/rules/gcp/gcs/no_lifecycle_policy"
)
