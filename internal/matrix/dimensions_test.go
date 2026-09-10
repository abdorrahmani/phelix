package matrix

import (
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
)

func TestCombinationDimensions_IdentityIsDeterministic(t *testing.T) {
	c := Combination{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	want := "arch=amd64/lang=go/os=linux/version=1.27"
	for i := 0; i < 10; i++ {
		if got := c.Identity(); got != want {
			t.Fatalf("identity = %q, want %q", got, want)
		}
	}
}

func TestCombinationDimensions_IdentityIncludesVariant(t *testing.T) {
	c := Combination{Lang: builder.Rust, Version: "1.77", OS: "linux", Arch: "arm", Variant: "v7", Platform: "linux/arm/v7"}
	want := "arch=arm/lang=rust/os=linux/variant=v7/version=1.77"
	if got := c.Identity(); got != want {
		t.Fatalf("identity = %q, want %q", got, want)
	}
}

func TestCombinationDimensions_SameLogicalCombinationAcrossRuns(t *testing.T) {
	// The same logical combination must produce the same identity regardless
	// of which run (or metadata) carries it.
	a := Combination{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	b := Combination{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "amd64", Platform: "linux/amd64",
		Metadata: map[string]string{"tag": "latest"}}
	if a.Identity() != b.Identity() {
		t.Fatalf("identities differ: %q vs %q", a.Identity(), b.Identity())
	}
}

func TestCombinationDimensions_ViewIsACopy(t *testing.T) {
	c := Combination{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	d := c.Dimensions()
	d[DimVersion] = "9.99"
	if c.Version != "1.27" {
		t.Fatal("mutating the dimension view changed the combination")
	}
}

func TestDimensionNames_Listed(t *testing.T) {
	names := DimensionNames()
	if len(names) != 5 {
		t.Fatalf("dimension names = %v", names)
	}
	for _, n := range names {
		if !isDimensionName(n) {
			t.Fatalf("unsupported dimension in list: %q", n)
		}
	}
}

func TestNewCombination_ValidatesInputs(t *testing.T) {
	if _, err := NewCombination(builder.Go, "1.27", "linux/amd64"); err != nil {
		t.Fatalf("valid combination rejected: %v", err)
	}
	if _, err := NewCombination(builder.Go, "not-a-version", "linux/amd64"); err == nil {
		t.Fatal("invalid version accepted")
	}
	if _, err := NewCombination(builder.Go, "1.27", "plan9/amd64"); err == nil {
		t.Fatal("unknown platform accepted")
	}
	c, err := NewCombination(builder.Go, "go1.26", "LINUX/ARM/V7")
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "1.26" || c.Platform != "linux/arm/v7" || c.Variant != "v7" {
		t.Fatalf("normalization: %+v", c)
	}
}
