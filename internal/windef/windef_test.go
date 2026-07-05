package windef

import "testing"

func TestExportable(t *testing.T) {
	drop := []string{"??_C@_05abc@hi@", "__real@3f800000", "__xmm@0", "main", "DllMain", "mainCRTStartup"}
	for _, s := range drop {
		if exportable(s) {
			t.Errorf("exportable(%q)=true, want false", s)
		}
	}
	keep := []string{"?greet@Greeter@foo@@QEAAXZ", "asm_add", "add", "?g_answer@foo@@3HA"}
	for _, s := range keep {
		if !exportable(s) {
			t.Errorf("exportable(%q)=false, want true", s)
		}
	}
}
