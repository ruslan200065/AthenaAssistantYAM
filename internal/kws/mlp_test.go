package kws

import (
	"encoding/json"
	"math"
	"testing"
)

func TestMLPForwardAndLayerNorm(t *testing.T) {
	w1 := make([]float32, Ctx*EmbDim*2)
	m := &MLP{
		Layers: []MLPLayer{
			{In: Ctx * EmbDim, Out: 2, W: w1, B: []float32{1, 3}, Gamma: []float32{1, 1}, Beta: []float32{0, 0}, Eps: 1e-5},
			{In: 2, Out: 1, W: []float32{1, -1}, B: []float32{0}},
		},
		Sigmoid: true,
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	got := m.Score(make([]float32, Ctx*EmbDim))
	want := float32(1 / (1 + math.Exp(2)))
	if math.Abs(float64(got-want)) > 1e-5 {
		t.Fatalf("score=%v want %v", got, want)
	}
}

func TestReadModelAcceptsMLP(t *testing.T) {
	m := Model{
		Name: "Афина",
		MLP: &MLP{
			Layers: []MLPLayer{
				{In: Ctx * EmbDim, Out: 1, W: make([]float32, Ctx*EmbDim), B: []float32{0}},
			},
			Sigmoid: true,
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadModel(b); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadModel([]byte(`{"name":"x"}`)); err == nil {
		t.Fatal("model without head/mlp accepted")
	}
}
