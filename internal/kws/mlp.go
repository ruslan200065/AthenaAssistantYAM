package kws

import (
	"errors"
	"fmt"
	"math"
)

// MLPLayer is one dense layer of a pre-trained wake-word model.
//
// Weights are stored row-major in the ONNX Gemm orientation: W is [Out][In],
// so y[o] = b[o] + sum(x[i] * W[o*In+i]).  A layer may carry an optional
// LayerNorm (gamma/beta) and a ReLU.
type MLPLayer struct {
	In    int       `json:"in"`
	Out   int       `json:"out"`
	W     []float32 `json:"w"`
	B     []float32 `json:"b"`
	Gamma []float32 `json:"gamma,omitempty"`
	Beta  []float32 `json:"beta,omitempty"`
	Eps   float32   `json:"eps,omitempty"`
	Relu  bool      `json:"relu,omitempty"`
}

// MLP is a compact pre-trained classifier over Ctx openWakeWord embeddings.
// The user's Athena model is a 1536 -> 64 -> 64 -> 1 network with two
// LayerNorm+ReLU blocks; keeping the weights in JSON avoids shipping a
// TensorFlow/ONNX runtime to the station.
type MLP struct {
	Layers  []MLPLayer `json:"layers"`
	Sigmoid bool       `json:"sigmoid"`
}

func (m *MLP) Validate() error {
	if len(m.Layers) == 0 {
		return errors.New("kws: mlp has no layers")
	}
	in := Ctx * EmbDim
	for i, l := range m.Layers {
		if l.In != in {
			return fmt.Errorf("kws: mlp layer %d input %d, want %d", i, l.In, in)
		}
		if l.Out < 1 {
			return fmt.Errorf("kws: mlp layer %d output %d", i, l.Out)
		}
		if len(l.W) != l.In*l.Out {
			return fmt.Errorf("kws: mlp layer %d weights %d, want %d", i, len(l.W), l.In*l.Out)
		}
		if len(l.B) != l.Out {
			return fmt.Errorf("kws: mlp layer %d bias %d, want %d", i, len(l.B), l.Out)
		}
		if len(l.Gamma) != 0 && len(l.Gamma) != l.Out {
			return fmt.Errorf("kws: mlp layer %d gamma %d, want %d", i, len(l.Gamma), l.Out)
		}
		if len(l.Beta) != 0 && len(l.Beta) != l.Out {
			return fmt.Errorf("kws: mlp layer %d beta %d, want %d", i, len(l.Beta), l.Out)
		}
		in = l.Out
	}
	if in != 1 {
		return fmt.Errorf("kws: mlp output width %d, want 1", in)
	}
	return nil
}

// Score returns the wake-word probability for a window of Ctx embeddings.
func (m *MLP) Score(x []float32) float32 {
	cur := x
	for _, l := range m.Layers {
		out := make([]float32, l.Out)
		for o := range out {
			row := l.W[o*l.In : (o+1)*l.In]
			var v float32
			for i, w := range row {
				v += cur[i] * w
			}
			out[o] = v + l.B[o]
		}
		if len(l.Gamma) == l.Out {
			eps := l.Eps
			if eps <= 0 {
				eps = 1e-5
			}
			var mean float32
			for _, v := range out {
				mean += v
			}
			mean /= float32(len(out))
			var variance float32
			for _, v := range out {
				d := v - mean
				variance += d * d
			}
			variance /= float32(len(out))
			inv := float32(1 / math.Sqrt(float64(variance+eps)))
			for i, v := range out {
				out[i] = (v-mean)*inv*l.Gamma[i] + l.Beta[i]
			}
		}
		if l.Relu {
			for i, v := range out {
				if v < 0 {
					out[i] = 0
				}
			}
		}
		cur = out
	}
	z := cur[0]
	if m.Sigmoid {
		return sigmoid(z)
	}
	return z
}
