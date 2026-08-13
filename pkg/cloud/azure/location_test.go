package azure

import "testing"

func TestLocationRegionCanonicalisesAzureDisplayNames(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"West Europe", "westeurope"},
		{"westeurope", "westeurope"},
		{"West Europe ", "westeurope"},
		{"East US", "eastus"},
		{"UK South", "uksouth"},
		{"global", "global"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := locationRegion(tt.in); got != tt.want {
			t.Errorf("locationRegion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
