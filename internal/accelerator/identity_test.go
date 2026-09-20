package accelerator

import "testing"

func TestSameName(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"H100", "H100", true},
		{"H100", "NVIDIA-H100-80GB-HBM3", true},
		{"NVIDIA-H100-80GB-HBM3", "H100", true},
		{"H100", "h100", true},
		{"L4", "nvidia-l4", true}, // GKE accelerator value vs operator key
		{"T4", "nvidia-tesla-t4", true},
		{"Gaudi-2", "Intel-Gaudi-2-96GB", true},
		{"H100", "A100", false},
		{"Gaudi-2", "Gaudi-3", false},
		// Two long names never meet through their short forms: GKE's Tesla
		// family reduces to one segment, and that must not make a T4 an A100.
		{"nvidia-tesla-t4", "nvidia-tesla-a100", false},
		{"NVIDIA-H100-80GB-HBM3", "nvidia-h100-80gb", false},
	}
	for _, c := range cases {
		if got := SameName(c.a, c.b); got != c.want {
			t.Errorf("SameName(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestFindKey(t *testing.T) {
	known := map[string]int{"H100": 1, "Gaudi-2": 2, "L4": 3, "nvidia-tesla-t4": 4}
	cases := []struct {
		name    string
		wantKey string
		wantOK  bool
	}{
		{"H100", "H100", true},
		{"NVIDIA-H100-80GB-HBM3", "H100", true},
		{"Gaudi-2", "Gaudi-2", true}, // exact first: its short name "2" matches nothing
		{"nvidia-l4", "L4", true},    // case fold, the tier the others miss
		{"nvidia-tesla-a100", "", false},
		{"t4", "nvidia-tesla-t4", true},
		{"MI300X", "", false},
	}
	for _, c := range cases {
		key, ok := FindKey(known, c.name)
		if key != c.wantKey || ok != c.wantOK {
			t.Errorf("FindKey(%q) = (%q, %v), want (%q, %v)", c.name, key, ok, c.wantKey, c.wantOK)
		}
	}

	// Several case-insensitive matches resolve deterministically.
	ambiguous := map[string]int{"h100": 1, "H100": 2}
	if key, _ := FindKey(ambiguous, "NVIDIA-H100-PCIE-80GB"); key != "H100" {
		t.Errorf("FindKey over an ambiguous map = %q, want the exact short name H100", key)
	}
	if key, _ := FindKey(map[string]int{"h100": 1, "H100x": 2}, "H100X"); key != "H100x" {
		t.Errorf("case-insensitive exact match should win: got %q", key)
	}
}

func TestNormalizeTeslaFamily(t *testing.T) {
	cases := map[string]string{
		"nvidia-tesla-t4":      "t4",
		"nvidia-tesla-a100":    "a100",
		"NVIDIA-Tesla-V100":    "V100",
		"nvidia-l4":            "l4",
		"nvidia-h100-80gb":     "h100",
		"Tesla-V100-SXM2-16GB": "V100", // NFD spelling, no vendor prefix: the old fallback
	}
	for in, want := range cases {
		if got := NormalizeAcceleratorName(in); got != want {
			t.Errorf("NormalizeAcceleratorName(%q) = %q, want %q", in, got, want)
		}
	}
}
