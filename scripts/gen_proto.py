#!/usr/bin/env python3
"""
Generate .proto files from extracted Cursor schema JSON.

Emit ONE .proto file per top-level namespace (agent.v1, aiserver.v1), preserving
Cursor's nested message layout. Namespaces cross-reference via `import`.

Modes:
    --mode core        : minimal set for chat + agent + tool calling (transitive closure)
    --mode full        : everything
"""
import argparse
import json
import re
import sys
from pathlib import Path
from collections import OrderedDict, defaultdict

ROOT = Path(__file__).resolve().parent.parent
SCHEMA_PATH = ROOT / "captures" / "schema-3.10.20.raw.json"
PROTO_DIR = ROOT / "proto"

SCALAR = {
    1: "double", 2: "float", 3: "int64", 4: "uint64", 5: "int32",
    6: "fixed64", 7: "fixed32", 8: "bool", 9: "string", 12: "bytes",
    13: "uint32", 15: "sfixed32", 16: "sfixed64", 17: "sint32", 18: "sint64",
}

CORE_ROOTS = {
    "agent.v1.AgentRunRequest",
    "agent.v1.AgentServerMessage",
    "agent.v1.ExecClientMessage",
    "agent.v1.ExecServerMessage",
    "agent.v1.InteractionUpdate",
    "aiserver.v1.AvailableModelsRequest",
    "aiserver.v1.AvailableModelsResponse",
    "aiserver.v1.BidiAppendRequest",
    "aiserver.v1.BidiAppendResponse",
    "aiserver.v1.GetDefaultModelRequest",
    "aiserver.v1.GetDefaultModelResponse",
    "aiserver.v1.ErrorDetails",
    "aiserver.v1.GetServerConfigRequest",
    "aiserver.v1.GetServerConfigResponse",
}

# proto3 reserved words that can't appear as field names
PROTO_RESERVED = {"true", "false", "return", "if", "else", "message", "enum", "package", "syntax"}


def parse_namespace(tn: str):
    """agent.v1.Foo.Bar → ('agent.v1', 'Foo.Bar')"""
    parts = tn.split(".")
    # Namespace is always first two segments (e.g. "agent.v1")
    ns = ".".join(parts[:2])
    local = ".".join(parts[2:])
    return ns, local


def build_nested_tree(names):
    """Turn a flat set of dotted names into a nested dict.
    E.g. ['Foo', 'Foo.Bar', 'Foo.Bar.Baz'] → {'Foo': {'Bar': {'Baz': {}}}}"""
    tree = {}
    for name in sorted(names):
        parts = name.split(".")
        node = tree
        for p in parts:
            node = node.setdefault(p, {})
    return tree


def transitive_closure(messages, enums, roots):
    seen_msgs = set()
    seen_enums = set()
    stack = list(roots)
    while stack:
        tn = stack.pop()
        if tn in seen_msgs or tn in seen_enums:
            continue
        if tn in messages:
            seen_msgs.add(tn)
            for f in messages[tn]:
                if f.get("kind") in ("message", "enum"):
                    ref = f.get("T_name")
                    if ref:
                        stack.append(ref)
                v = f.get("V")
                if v and v.get("kind") in ("message", "enum"):
                    ref = v.get("T_name")
                    if ref:
                        stack.append(ref)
        elif tn in enums:
            seen_enums.add(tn)
    return seen_msgs, seen_enums


def type_ref_from_field(f, current_ns, current_scope, all_msgs, all_enums):
    """Resolve a field's type name to a proto reference relative to current scope."""
    kind = f.get("kind")
    if kind == "scalar":
        return SCALAR.get(f["T"], f'/* scalar_{f["T"]} */')
    if kind in ("message", "enum"):
        tn = f.get("T_name")
        if not tn:
            return "bytes"  # unresolved
        ref_ns, ref_local = parse_namespace(tn)
        if ref_ns == current_ns:
            # Same namespace: refer by local name.
            # If ref is a direct child of current scope, use short name.
            # Otherwise use fully qualified local name.
            return ref_local.split(".")[-1] if is_direct_child(ref_local, current_scope) else ref_local
        else:
            # Cross-namespace: use fully qualified name
            return tn
    if kind == "map":
        kt = SCALAR.get(f.get("K"), "string")
        v = f.get("V", {})
        if v.get("kind") == "scalar":
            vt = SCALAR.get(v.get("T"), "string")
        elif v.get("kind") in ("message", "enum"):
            tn = v.get("T_name")
            if tn:
                ref_ns, ref_local = parse_namespace(tn)
                vt = tn if ref_ns != current_ns else ref_local
            else:
                vt = "bytes"
        else:
            vt = "bytes"
        return f"map<{kt}, {vt}>"
    return "bytes"


def is_direct_child(candidate: str, parent: str) -> bool:
    """Is `candidate` a direct child of `parent`? e.g. child='Foo.Bar', parent='Foo' → True."""
    if not parent:
        return "." not in candidate
    if not candidate.startswith(parent + "."):
        return False
    remainder = candidate[len(parent) + 1:]
    return "." not in remainder


def sanitize_enum_value(name: str) -> str:
    if not name:
        return "UNSPECIFIED"
    if name[0].isdigit():
        return "_" + name
    return name


def emit_nested_message_unified(local_name, msg_local_key, messages, enums, include_msgs_of_ns, include_enums_of_ns, indent, ns, wrapper_name):
    """Emit a message nested inside its namespace wrapper. Cross-namespace refs
    use `{OtherWrapper}.LocalName` fully qualified from the umbrella package."""
    lines = []
    tn = f"{ns}.{msg_local_key}"
    if tn not in messages:
        return lines

    pad = "  " * indent
    fields = messages[tn]

    my_prefix = msg_local_key + "."
    ns_prefix = f"{ns}.{my_prefix}"
    # Find nested messages directly under this scope
    nested_msg_tns = [k for k in include_msgs_of_ns
                      if k.startswith(ns_prefix) and
                      k[len(ns_prefix):].count(".") == 0]
    nested_enum_tns = [k for k in include_enums_of_ns
                       if k.startswith(ns_prefix) and
                       k[len(ns_prefix):].count(".") == 0]

    lines.append(f'{pad}message {local_name} {{')

    for en_tn in nested_enum_tns:
        _, en_local = parse_namespace(en_tn)
        en_short = en_local.split(".")[-1]
        lines.extend(emit_enum(en_tn, en_short, enums, indent + 1))

    for child_tn in nested_msg_tns:
        _, child_local = parse_namespace(child_tn)
        child_short = child_local.split(".")[-1]
        lines.extend(emit_nested_message_unified(child_short, child_local, messages, enums, include_msgs_of_ns, include_enums_of_ns, indent + 1, ns, wrapper_name))

    oneofs = OrderedDict()
    singles = []
    for f in fields:
        if f.get("oneof"):
            oneofs.setdefault(f["oneof"], []).append(f)
        else:
            singles.append(f)

    singles.sort(key=lambda x: x["no"])

    for f in singles:
        t = type_ref_unified(f, ns, msg_local_key, wrapper_name)
        mods = []
        if f.get("repeated"):
            mods.append("repeated")
        elif f.get("opt") and not t.startswith("map<"):
            mods.append("optional")
        mod = " ".join(mods) + " " if mods else ""
        name = f["name"]
        if name in PROTO_RESERVED:
            name = name + "_"
        lines.append(f'  {pad}{mod}{t} {name} = {f["no"]};')

    for oname, ofields in oneofs.items():
        lines.append(f'  {pad}oneof {oname} {{')
        for f in sorted(ofields, key=lambda x: x["no"]):
            t = type_ref_unified(f, ns, msg_local_key, wrapper_name)
            name = f["name"]
            if name in PROTO_RESERVED:
                name = name + "_"
            lines.append(f'    {pad}{t} {name} = {f["no"]};')
        lines.append(f'  {pad}}}')

    lines.append(f'{pad}}}')
    lines.append('')
    return lines


def type_ref_unified(f, current_ns, current_scope, current_wrapper):
    """Resolve type ref for unified single-package output.
    Same namespace: use `Wrapper.Local` (from top of package).
    Cross namespace: use `OtherWrapper.Local`."""
    kind = f.get("kind")
    if kind == "scalar":
        return SCALAR.get(f["T"], f'/* scalar_{f["T"]} */')
    if kind in ("message", "enum"):
        tn = f.get("T_name")
        if not tn:
            return "bytes"
        ref_ns, ref_local = parse_namespace(tn)
        # Compute wrapper for target
        parts = ref_ns.split(".")
        target_wrapper = "".join(p[0].upper() + p[1:] for p in parts)
        return f"{target_wrapper}.{ref_local}"
    if kind == "map":
        kt = SCALAR.get(f.get("K"), "string")
        v = f.get("V", {})
        if v.get("kind") == "scalar":
            vt = SCALAR.get(v.get("T"), "string")
        elif v.get("kind") in ("message", "enum"):
            tn = v.get("T_name")
            if tn:
                ref_ns, ref_local = parse_namespace(tn)
                parts = ref_ns.split(".")
                target_wrapper = "".join(p[0].upper() + p[1:] for p in parts)
                vt = f"{target_wrapper}.{ref_local}"
            else:
                vt = "bytes"
        else:
            vt = "bytes"
        return f"map<{kt}, {vt}>"
    return "bytes"


def emit_nested_message(local_name, current_scope, messages, enums, include_msgs, include_enums, indent, ns, tree_root, msg_local_key):
    """Emit a message and its nested children recursively."""
    lines = []
    tn = f"{ns}.{msg_local_key}" if msg_local_key else None
    if not tn or tn not in messages:
        return lines

    pad = "  " * indent
    fields = messages[tn]

    # Find nested messages and enums directly under this scope
    my_prefix = msg_local_key + "."
    nested_msg_locals = [k for k in include_msgs
                         if k.startswith(f"{ns}.{my_prefix}") and
                         k[len(f"{ns}.{my_prefix}"):].count(".") == 0]
    nested_enum_locals = [k for k in include_enums
                          if k.startswith(f"{ns}.{my_prefix}") and
                          k[len(f"{ns}.{my_prefix}"):].count(".") == 0]

    lines.append(f'{pad}message {local_name} {{')

    # Emit nested enums first
    for en in nested_enum_locals:
        _, en_local = parse_namespace(en)
        en_short = en_local.split(".")[-1]
        lines.extend(emit_enum(en, en_short, enums, indent + 1))

    # Emit nested messages
    for child_tn in nested_msg_locals:
        _, child_local = parse_namespace(child_tn)
        child_short = child_local.split(".")[-1]
        lines.extend(emit_nested_message(child_short, child_local, messages, enums, include_msgs, include_enums, indent + 1, ns, tree_root, child_local))

    # Emit fields
    oneofs = OrderedDict()
    singles = []
    for f in fields:
        if f.get("oneof"):
            oneofs.setdefault(f["oneof"], []).append(f)
        else:
            singles.append(f)

    singles.sort(key=lambda x: x["no"])

    for f in singles:
        t = type_ref_from_field(f, ns, msg_local_key, messages, enums)
        mods = []
        if f.get("repeated"):
            mods.append("repeated")
        elif f.get("opt") and not t.startswith("map<"):
            mods.append("optional")
        mod = " ".join(mods) + " " if mods else ""
        name = f["name"]
        if name in PROTO_RESERVED:
            name = name + "_"
        lines.append(f'  {pad}{mod}{t} {name} = {f["no"]};')

    for oname, ofields in oneofs.items():
        lines.append(f'  {pad}oneof {oname} {{')
        for f in sorted(ofields, key=lambda x: x["no"]):
            t = type_ref_from_field(f, ns, msg_local_key, messages, enums)
            name = f["name"]
            if name in PROTO_RESERVED:
                name = name + "_"
            lines.append(f'    {pad}{t} {name} = {f["no"]};')
        lines.append(f'  {pad}}}')

    lines.append(f'{pad}}}')
    lines.append('')
    return lines


def emit_enum(tn, local_name, enums, indent):
    lines = []
    pad = "  " * indent
    values = enums.get(tn, [])
    lines.append(f'{pad}enum {local_name} {{')
    seen = set()
    values_sorted = sorted(values, key=lambda v: v["no"])
    # Ensure first value is 0 (proto3 requirement)
    if not values_sorted or values_sorted[0]["no"] != 0:
        lines.append(f'  {pad}{local_name.upper()}_UNSPECIFIED = 0;')
    for v in values_sorted:
        n = sanitize_enum_value(v["name"])
        if n in seen:
            continue
        seen.add(n)
        lines.append(f'  {pad}{n} = {v["no"]};')
    lines.append(f'{pad}}}')
    lines.append('')
    return lines


def emit_namespace(ns: str, messages, enums, include_msgs, include_enums, cross_namespaces):
    lines = ['syntax = "proto3";']
    lines.append('')
    lines.append(f'package {ns};')
    lines.append('')
    ns_slug = ns.replace(".", "_")
    lines.append(f'option go_package = "github.com/router-for-me/cursor-proto/gen/{ns_slug};{ns_slug}";')

    # Imports
    for other_ns in sorted(cross_namespaces):
        if other_ns == ns:
            continue
        other_slug = other_ns.replace(".", "_")
        lines.append(f'import "{other_slug}.proto";')
    lines.append('')
    lines.append(f'// Generated from Cursor 3.10.20 workbench.desktop.main.js')
    lines.append('')

    # Find top-level messages and enums (those with no `.` in their local name)
    top_msgs = []
    top_enums = []
    for tn in include_msgs:
        n, local = parse_namespace(tn)
        if n == ns and "." not in local:
            top_msgs.append((tn, local))
    for tn in include_enums:
        n, local = parse_namespace(tn)
        if n == ns and "." not in local:
            top_enums.append((tn, local))

    top_msgs.sort(key=lambda x: x[1])
    top_enums.sort(key=lambda x: x[1])

    # Top-level enums
    for tn, local in top_enums:
        lines.extend(emit_enum(tn, local, enums, 0))

    # Top-level messages
    for tn, local in top_msgs:
        lines.extend(emit_nested_message(local, local, messages, enums, include_msgs, include_enums, 0, ns, None, local))

    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", choices=["core", "full"], default="core")
    ap.add_argument("--schema", default=str(SCHEMA_PATH),
                    help="path to schema-<cursor>.raw.json (defaults to 3.10.20)")
    args = ap.parse_args()

    schema = json.load(open(args.schema))
    messages = schema["messages"]
    enums = schema["enums"]

    if args.mode == "full":
        include_msgs = set(messages.keys())
        include_enums = set(enums.keys())
    else:
        include_msgs, include_enums = transitive_closure(messages, enums, CORE_ROOTS)

    # Group by namespace
    ns_to_msgs = defaultdict(set)
    ns_to_enums = defaultdict(set)
    for tn in include_msgs:
        ns, _ = parse_namespace(tn)
        ns_to_msgs[ns].add(tn)
    for tn in include_enums:
        ns, _ = parse_namespace(tn)
        ns_to_enums[ns].add(tn)

    namespaces = set(ns_to_msgs.keys()) | set(ns_to_enums.keys())

    # Only remove the file we're about to regenerate. Historically this
    # wiped every .proto in proto/, which nuked hand-authored siblings
    # like cursor_usage.proto — see docs/kernel-3.11-upgrade.md for the
    # incident that cost us one debug cycle.
    target = PROTO_DIR / "cursor.proto"
    if target.exists():
        target.unlink()

    # Emit one file per namespace, no cross-file imports (both packages live in
    # separate files but the resolver treats them as one compilation unit when
    # invoked with a shared proto_path).
    #
    # Since protoc doesn't allow cyclic imports between .proto files, we merge
    # everything into a single file with multiple `package` blocks... but
    # protoc doesn't support that either — one .proto = one package.
    #
    # Actual solution: single file, single package. We use `cursor` as the
    # umbrella package and prefix message names with the original namespace to
    # avoid collisions.
    out_path = PROTO_DIR / "cursor.proto"
    lines = ['syntax = "proto3";']
    lines.append('')
    lines.append('package cursor;')
    lines.append('')
    lines.append('option go_package = "github.com/router-for-me/cursor-proto/gen/cursor;cursorpb";')
    lines.append('')
    lines.append('// Generated from Cursor 3.10.20 workbench.desktop.main.js')
    lines.append(f'// commit: {schema["cursor_commit"]}')
    lines.append(f'// Mode: {args.mode}, {len(include_msgs)} messages, {len(include_enums)} enums')
    lines.append('')

    # For unified package, we emit each namespace as a top-level "wrapper" message
    # containing all its members as nested types. This preserves the original
    # namespace hierarchy exactly.
    for ns in sorted(namespaces):
        ns_slug = ns.replace(".", "_").title().replace("_", "")
        # e.g. agent.v1 → AgentV1
        parts = ns.split(".")
        wrapper_name = "".join(p[0].upper() + p[1:] for p in parts)  # AgentV1, AiserverV1
        lines.append(f'// ============================================================')
        lines.append(f'// Namespace: {ns} → {wrapper_name}')
        lines.append(f'// ============================================================')
        lines.append('')
        lines.append(f'message {wrapper_name} {{')

        # Top-level enums in this namespace, emitted as nested enums of the wrapper
        top_enums = sorted(
            [(tn, parse_namespace(tn)[1]) for tn in ns_to_enums.get(ns, set())
             if "." not in parse_namespace(tn)[1]],
            key=lambda x: x[1]
        )
        for tn, local in top_enums:
            lines.extend(emit_enum(tn, local, enums, 1))

        # Top-level messages, emitted as nested messages
        top_msgs = sorted(
            [(tn, parse_namespace(tn)[1]) for tn in ns_to_msgs.get(ns, set())
             if "." not in parse_namespace(tn)[1]],
            key=lambda x: x[1]
        )
        for tn, local in top_msgs:
            lines.extend(emit_nested_message_unified(local, local, messages, enums, ns_to_msgs.get(ns, set()), ns_to_enums.get(ns, set()), 1, ns, wrapper_name))

        lines.append('}')
        lines.append('')

    out_path.write_text("\n".join(lines))
    print(f"Wrote {out_path}")
    print(f"  Total messages: {len(include_msgs)}")
    print(f"  Total enums:    {len(include_enums)}")
    print(f"  Namespaces:     {sorted(namespaces)}")


if __name__ == "__main__":
    main()
