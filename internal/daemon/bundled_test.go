package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ruslan200065/AthenaAssistantYAM/internal/kws"
)

func TestBundledModelInstalledOnce(t *testing.T) {
	dir := t.TempDir()
	model := kws.Model{
		Name: "Афина", Threshold: 0.5,
		MLP: &kws.MLP{
			Layers: []kws.MLPLayer{
				{In: kws.Ctx * kws.EmbDim, Out: 1, W: make([]float32, kws.Ctx*kws.EmbDim), B: []float32{0}},
			},
			Sigmoid: true,
		},
	}
	b, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(dir, "athena.json")
	if err := os.WriteFile(modelPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := New(Options{DataDir: filepath.Join(dir, "data"), BundledModel: modelPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.words) != 1 || d.words[0].Model.Name != "Афина" {
		t.Fatalf("words=%+v", d.words)
	}
	if s := d.settings.Words["афина"]; s == nil || !s.Enabled {
		t.Fatalf("bundled word is not enabled: %+v", d.settings.Words)
	}

	// A second start must not overwrite or duplicate it.
	d2, err := New(Options{DataDir: filepath.Join(dir, "data"), BundledModel: modelPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.words) != 1 {
		t.Fatalf("duplicate words: %+v", d2.words)
	}
}
