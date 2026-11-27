#!/usr/bin/env python3
"""Convert this app's trained wake-word head JSON to an openWakeWord-style ONNX.

The app stores a small classifier over 16 openWakeWord embeddings (16 x 96).
This script reproduces exactly the same forward pass in ONNX:

    x[16,96] --Gemm(shared 96->24)--> ReLU --flatten[384]
             --Gemm(384->24)--> ReLU --Gemm(24->1)--> Sigmoid

Input:  float32[1,16,96]  (oldest embedding first)
Output: float32[1,1]      (wake-word probability)
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

import numpy as np
import onnx
from onnx import TensorProto, helper, numpy_helper


def tensor(name: str, value: np.ndarray) -> onnx.TensorProto:
    return numpy_helper.from_array(np.asarray(value, dtype=np.float32), name=name)


def convert(model_json: Path, output: Path) -> None:
    data = json.loads(model_json.read_text(encoding="utf-8"))
    head = data.get("head")
    if not head:
        raise SystemExit("model has no 'head' tensor set (MLP models are not supported)")

    # The Go trainer stores W1 as [96][24], W2 as [384][24], W3 as [24].
    # ONNX Gemm with transB=1 wants B in [out][in] orientation.
    w1 = np.asarray(head["w1"], dtype=np.float32).reshape(96, 24).T
    b1 = np.asarray(head["b1"], dtype=np.float32).reshape(24)
    w2 = np.asarray(head["w2"], dtype=np.float32).reshape(384, 24).T
    b2 = np.asarray(head["b2"], dtype=np.float32).reshape(24)
    w3 = np.asarray(head["w3"], dtype=np.float32).reshape(24, 1).T
    b3 = np.asarray(head["b3"], dtype=np.float32).reshape(1)

    initializers = [
        tensor("w1", w1),
        tensor("b1", b1),
        tensor("w2", w2),
        tensor("b2", b2),
        tensor("w3", w3),
        tensor("b3", b3),
        numpy_helper.from_array(np.asarray([16, 96], dtype=np.int64), name="shape_16x96"),
        numpy_helper.from_array(np.asarray([1, 384], dtype=np.int64), name="shape_1x384"),
    ]

    # Use the same tensor names as the reference ONNX supplied for the other
    # station. Loaders that address tensors by name keep working; loaders that
    # use input/output indexes are unaffected.
    input_name = "onnx::Flatten_0"
    output_name = "39"

    nodes = [
        # One shared projection per embedding, exactly like Head.forward.
        helper.make_node("Reshape", [input_name, "shape_16x96"], ["flat16"]),
        helper.make_node("Gemm", ["flat16", "w1", "b1"], ["a1"], transB=1),
        helper.make_node("Relu", ["a1"], ["r1"]),
        # Time-major flatten: t0[0..23], t1[0..23], ...
        helper.make_node("Reshape", ["r1", "shape_1x384"], ["flat384"]),
        helper.make_node("Gemm", ["flat384", "w2", "b2"], ["a2"], transB=1),
        helper.make_node("Relu", ["a2"], ["r2"]),
        helper.make_node("Gemm", ["r2", "w3", "b3"], ["logit"], transB=1),
        helper.make_node("Sigmoid", ["logit"], [output_name]),
    ]

    graph = helper.make_graph(
        nodes,
        "athena_wakeword",
        [helper.make_tensor_value_info(input_name, TensorProto.FLOAT, [1, 16, 96])],
        [helper.make_tensor_value_info(output_name, TensorProto.FLOAT, [1, 1])],
        initializer=initializers,
    )
    model = helper.make_model(
        graph,
        producer_name="AthenaAssistantYAM",
        opset_imports=[helper.make_opsetid("", 13)],
    )
    # The reference openWakeWord model used IR 7 / opset 13; keep the widest
    # compatibility with older ONNX runtimes on other stations.
    model.ir_version = 7
    model.metadata_props.add(key="name", value=str(data.get("name", "")))
    model.metadata_props.add(key="threshold", value=str(data.get("threshold", "")))
    model.metadata_props.add(key="takes", value=str(data.get("takes", "")))
    model.metadata_props.add(key="input_layout", value="[1,16,96] float32, oldest first")
    model.metadata_props.add(key="output_layout", value="[1,1] float32 sigmoid")

    onnx.checker.check_model(model)
    output.parent.mkdir(parents=True, exist_ok=True)
    onnx.save(model, output)
    print(f"saved {output} ({output.stat().st_size} bytes)")

    # Numerical self-check: ONNX Runtime must reproduce the JSON formula.
    import onnxruntime as ort

    rng = np.random.default_rng(20260524)
    x = rng.standard_normal((1, 16, 96), dtype=np.float32)
    sess = ort.InferenceSession(str(output), providers=["CPUExecutionProvider"])
    got = float(sess.run(None, {input_name: x})[0][0, 0])

    xf = x.reshape(16, 96)
    a1 = np.maximum(xf @ np.asarray(head["w1"], dtype=np.float32).reshape(96, 24) + b1, 0)
    a2 = np.maximum(a1.reshape(1, 384) @ np.asarray(head["w2"], dtype=np.float32).reshape(384, 24) + b2, 0)
    z = float(a2.reshape(24) @ np.asarray(head["w3"], dtype=np.float32).reshape(24) + b3[0])
    want = 1.0 / (1.0 + np.exp(-z))
    print(f"onnx={got:.9f} json={want:.9f} delta={abs(got - want):.3g}")
    if abs(got - want) > 1e-5:
        raise SystemExit("ONNX output does not match the JSON head")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("model_json", type=Path)
    ap.add_argument("output", type=Path)
    args = ap.parse_args()
    convert(args.model_json, args.output)


if __name__ == "__main__":
    main()
