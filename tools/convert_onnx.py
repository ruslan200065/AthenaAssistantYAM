#!/usr/bin/env python3
"""Convert the user's Athena ONNX wake-word model to the app's JSON format.

The ONNX graph is a small MLP over openWakeWord embeddings:

    Flatten(1,16,96) -> Gemm -> LayerNorm -> ReLU
                     -> Gemm -> LayerNorm -> ReLU
                     -> Gemm -> Sigmoid

Keeping the weights as plain JSON lets the station run the model with a few
dozen lines of Go instead of shipping an ONNX/TFLite runtime for aarch64.
"""
import argparse
import json
from pathlib import Path

import numpy as np
import onnx
from onnx import numpy_helper


def tensor(graph, name):
    for init in graph.initializer:
        if init.name == name:
            return numpy_helper.to_array(init)
    for node in graph.node:
        if node.op_type == "Constant" and node.output[0] == name:
            return numpy_helper.to_array(node.attribute[0].t)
    raise KeyError(name)


def node_by_name(graph, name):
    for node in graph.node:
        if node.name == name:
            return node
    raise KeyError(name)


def gemm_layer(graph, node_name, relu=True):
    node = node_by_name(graph, node_name)
    weight = tensor(graph, node.input[1]).astype(np.float32)  # [out, in]
    bias = tensor(graph, node.input[2]).astype(np.float32)
    out, inp = weight.shape
    layer = {
        "in": int(inp),
        "out": int(out),
        "w": weight.reshape(-1).tolist(),
        "b": bias.reshape(-1).tolist(),
        "relu": bool(relu),
    }
    return layer, node.output[0]


def add_norm(graph, layer, outputs):
    """Attach the LayerNorm parameters that follow a Gemm output."""
    # outputs: [mean, sub, pow, mean2, add, sqrt, div, mul, add2]
    # Find Mul input that is not from Div (gamma) and Add_1 input that is not
    # from Mul (beta).
    mul = node_by_name(graph, outputs["mul"])
    div = node_by_name(graph, outputs["div"])
    add2 = node_by_name(graph, outputs["add2"])
    gamma_name = next(n for n in mul.input if n != div.output[0])
    beta_name = next(n for n in add2.input if n != mul.output[0])
    constant_name = node_by_name(graph, outputs["add"]).input[1]
    eps = float(tensor(graph, constant_name))
    layer["gamma"] = tensor(graph, gamma_name).astype(np.float32).reshape(-1).tolist()
    layer["beta"] = tensor(graph, beta_name).astype(np.float32).reshape(-1).tolist()
    layer["eps"] = eps
    return layer


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("onnx")
    ap.add_argument("output")
    ap.add_argument("--name", default="Афина")
    ap.add_argument("--threshold", type=float, default=0.5)
    args = ap.parse_args()

    model = onnx.load(args.onnx)
    graph = model.graph

    inputs = [x for x in graph.input]
    outputs = [x for x in graph.output]
    dims = [d.dim_value for d in inputs[0].type.tensor_type.shape.dim]
    if dims != [1, 16, 96]:
        raise SystemExit(f"unexpected input shape: {dims} (want [1,16,96])")
    if outputs[0].type.tensor_type.shape.dim[-1].dim_value != 1:
        raise SystemExit("unexpected output shape")

    l1, o1 = gemm_layer(graph, "/layer1/Gemm", relu=True)
    add_norm(graph, l1, {
        "mean": "/layernorm1/ReduceMean",
        "sub": "/layernorm1/Sub",
        "pow": "/layernorm1/Pow",
        "mean2": "/layernorm1/ReduceMean_1",
        "add": "/layernorm1/Add",
        "sqrt": "/layernorm1/Sqrt",
        "div": "/layernorm1/Div",
        "mul": "/layernorm1/Mul",
        "add2": "/layernorm1/Add_1",
    })
    l2, _ = gemm_layer(graph, "/blocks.0/fcn_layer/Gemm", relu=True)
    add_norm(graph, l2, {
        "mean": "/blocks.0/layer_norm/ReduceMean",
        "sub": "/blocks.0/layer_norm/Sub",
        "pow": "/blocks.0/layer_norm/Pow",
        "mean2": "/blocks.0/layer_norm/ReduceMean_1",
        "add": "/blocks.0/layer_norm/Add",
        "sqrt": "/blocks.0/layer_norm/Sqrt",
        "div": "/blocks.0/layer_norm/Div",
        "mul": "/blocks.0/layer_norm/Mul",
        "add2": "/blocks.0/layer_norm/Add_1",
    })
    l3, _ = gemm_layer(graph, "/last_layer/Gemm", relu=False)
    if l3["out"] != 1:
        raise SystemExit("last layer is not 1-wide")

    app_model = {
        "name": args.name,
        "threshold": args.threshold,
        "created": "2026-05-24T20:58:28Z",
        "takes": 0,
        "recall": 0,
        "false_per_h": 0,
        "eval_hours": 0,
        "notes": ["Готовая модель, конвертирована из ONNX."],
        "mlp": {
            "layers": [l1, l2, l3],
            "sigmoid": True,
        },
    }
    out = Path(args.output)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", encoding="utf-8") as fh:
        json.dump(app_model, fh, ensure_ascii=False, separators=(",", ":"))
    print(f"wrote {out} ({out.stat().st_size} bytes)")


if __name__ == "__main__":
    main()
