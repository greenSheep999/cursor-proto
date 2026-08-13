#!/usr/bin/env python3
"""
Extract Cursor's protobuf schema from workbench.desktop.main.js.

Cursor uses @bufbuild/protobuf v2 API:
    XXX = <ns>.makeMessageType("<typeName>", [ ...fields ])
    or
    XXX = <ns>.makeMessageType("<typeName>", () => [ ...fields ])

Enums:
    XXX = <ns>.makeEnum("<typeName>", [{no:0,name:"..."}, ...])

Services:
    XXX = { typeName: "...", methods: {...} }
"""
import os
import re
import json
import sys
from pathlib import Path
from collections import OrderedDict

# Resolve the workbench source relative to this script so the extractor
# works from any checkout without hard-coding an absolute path.
_REPO_ROOT = Path(__file__).resolve().parent.parent
WB = Path(os.environ.get("CURSOR_PROTO_WB", _REPO_ROOT / "captures" / "wb-3.10.20.js"))
CURSOR_VERSION = os.environ.get("CURSOR_PROTO_VERSION", "3.10.20")
CURSOR_COMMIT = os.environ.get("CURSOR_PROTO_COMMIT", "23b9fb205fe595ea2be29da7214e19762d037fc0")
BASE_SCHEMA = os.environ.get("CURSOR_PROTO_BASE_SCHEMA", "")

SCALAR_TYPES = {
    1: "double", 2: "float", 3: "int64", 4: "uint64", 5: "int32",
    6: "fixed64", 7: "fixed32", 8: "bool", 9: "string", 12: "bytes",
    13: "uint32", 14: "enum", 15: "sfixed32", 16: "sfixed64",
    17: "sint32", 18: "sint64",
}


def match_balanced(js: str, start: int, open_ch: str, close_ch: str):
    depth = 0
    i = start
    n = len(js)
    in_str = None
    escape = False
    while i < n:
        c = js[i]
        if in_str:
            if escape:
                escape = False
            elif c == '\\':
                escape = True
            elif c == in_str:
                in_str = None
        else:
            if c in '"\'`':
                in_str = c
            elif c == open_ch:
                depth += 1
            elif c == close_ch:
                depth -= 1
                if depth == 0:
                    return start, i
        i += 1
    return start, -1


def parse_field_entry(entry: str):
    result = {}
    m = re.search(r'\bno:\s*(\d+)', entry)
    if not m:
        return None
    result["no"] = int(m.group(1))

    m = re.search(r'\bname:\s*"([^"]+)"', entry)
    if m:
        result["name"] = m.group(1)

    m = re.search(r'\bkind:\s*"([^"]+)"', entry)
    if m:
        result["kind"] = m.group(1)

    # T can be:
    #   T: 9                     (scalar type code)
    #   T: () => Xxx             (message/enum ref via minified var)
    #   T: Xxx                   (message ref, bare)
    #   T: N.getEnumType(Xxx)    (enum ref via helper)
    m = re.search(r'\bT:\s*(?:\(\s*\)\s*=>\s*)?([A-Za-z_$][\w$]*(?:\.\w+)?(?:\([A-Za-z_$][\w$]*\))?|\d+)', entry)
    if m:
        t = m.group(1)
        if t.isdigit():
            result["T"] = int(t)
        else:
            # Strip helper calls like N.getEnumType(xc) → xc
            enum_m = re.search(r'getEnumType\(([A-Za-z_$][\w$]*)\)', t)
            if enum_m:
                result["T_ref"] = enum_m.group(1)
            else:
                result["T_ref"] = t

    m = re.search(r'\bK:\s*(\d+)', entry)
    if m:
        result["K"] = int(m.group(1))

    m = re.search(r'\bV:\s*\{([^}]*)\}', entry)
    if m:
        vbody = m.group(1)
        vk = re.search(r'\bkind:\s*"([^"]+)"', vbody)
        vt_num = re.search(r'\bT:\s*(\d+)(?!\d)', vbody)
        vt_ref = re.search(r'\bT:\s*(?:\(\s*\)\s*=>\s*)?([A-Za-z_$][\w$]*)', vbody)
        result["V"] = {}
        if vk:
            result["V"]["kind"] = vk.group(1)
        if vt_num:
            result["V"]["T"] = int(vt_num.group(1))
        elif vt_ref and not vt_ref.group(1).isdigit():
            result["V"]["T_ref"] = vt_ref.group(1)

    # JS 里可能写作 `repeated:!0`（=true）或 `repeated:true`
    if re.search(r'\brepeated:\s*(?:true|!0)\b', entry):
        result["repeated"] = True
    if re.search(r'\bopt:\s*(?:true|!0)\b', entry):
        result["opt"] = True

    m = re.search(r'\boneof:\s*"([^"]+)"', entry)
    if m:
        result["oneof"] = m.group(1)

    return result


def extract_field_list(js: str, list_start: int):
    """Given position of the `[` opening the field list, return parsed fields."""
    _, list_end = match_balanced(js, list_start, '[', ']')
    if list_end < 0:
        return None
    body = js[list_start + 1: list_end]

    fields = []
    i = 0
    n = len(body)
    while i < n:
        if body[i] == '{':
            _, end = match_balanced(body, i, '{', '}')
            if end < 0:
                break
            entry = body[i:end + 1]
            f = parse_field_entry(entry)
            if f:
                fields.append(f)
            i = end + 1
        else:
            i += 1
    return fields


def extract_all_messages(js: str):
    """Find every makeMessageType("<tn>", <fields>) call."""
    messages = OrderedDict()
    # Match: .makeMessageType("<typeName>",
    pat = re.compile(r'\.makeMessageType\(\s*"([a-zA-Z_][\w.]*)"\s*,')
    for m in pat.finditer(js):
        tn = m.group(1)
        # After the comma, expect `[` or `()=>[`
        pos = m.end()
        # Skip whitespace, optional `()=>` (with or without spaces)
        while pos < len(js) and js[pos] in ' \t\n':
            pos += 1
        # Match `()=>` or `() =>` or variants
        arrow_m = re.match(r'\(\s*\)\s*=>\s*', js[pos:pos + 20])
        if arrow_m:
            pos += arrow_m.end()
        while pos < len(js) and js[pos] in ' \t\n':
            pos += 1
        if pos >= len(js) or js[pos] != '[':
            continue
        fields = extract_field_list(js, pos)
        if fields is None:
            continue
        # If already seen, prefer the one with more fields (some are shadowed)
        if tn in messages and len(fields) < len(messages[tn]):
            continue
        messages[tn] = fields
    return messages


def extract_all_enums(js: str):
    """Find every makeEnum("<tn>", <values>) call."""
    enums = OrderedDict()
    pat = re.compile(r'\.makeEnum\(\s*"([a-zA-Z_][\w.]*)"\s*,\s*\[')
    for m in pat.finditer(js):
        tn = m.group(1)
        list_start = m.end() - 1  # position of `[`
        _, list_end = match_balanced(js, list_start, '[', ']')
        if list_end < 0:
            continue
        body = js[list_start + 1: list_end]
        values = []
        i = 0
        while i < len(body):
            if body[i] == '{':
                _, end = match_balanced(body, i, '{', '}')
                if end < 0:
                    break
                entry = body[i:end + 1]
                no_m = re.search(r'\bno:\s*(-?\d+)', entry)
                name_m = re.search(r'\bname:\s*"([^"]+)"', entry)
                if no_m and name_m:
                    values.append({"no": int(no_m.group(1)), "name": name_m.group(1)})
                i = end + 1
            else:
                i += 1
        enums[tn] = values
    return enums


def build_ref_map(js: str):
    """
    Map minified variable → typeName.
    Cursor code binds like:  XXX=<ns>.makeMessageType("...",[...])
    or                       XXX=<ns>.makeEnum("...",[...])
    """
    ref = {}
    pat = re.compile(
        r'\b([A-Za-z_$][\w$]*)\s*=\s*[A-Za-z_$][\w$]*\.make(?:MessageType|Enum)\(\s*"([a-zA-Z_][\w.]*)"'
    )
    for m in pat.finditer(js):
        var, tn = m.group(1), m.group(2)
        # Later bindings win only if shorter var → keep first
        ref.setdefault(var, tn)
    return ref


def resolve_refs(messages, enums, ref_map):
    """Replace T_ref with actual typeName if resolvable."""
    unresolved = set()
    for tn, fields in messages.items():
        for f in fields:
            if "T_ref" in f:
                r = f["T_ref"]
                if r in ref_map:
                    f["T_name"] = ref_map[r]
                else:
                    unresolved.add(r)
            if "V" in f and "T_ref" in f["V"]:
                r = f["V"]["T_ref"]
                if r in ref_map:
                    f["V"]["T_name"] = ref_map[r]
                else:
                    unresolved.add(r)
    return unresolved


def inherit_resolved_refs(messages, base_schema_path):
    """Restore type names for unchanged fields obscured by a new minifier.

    Cursor occasionally stops leaving a direct makeMessageType binding for a
    field's generated symbol. When the message name, field number, field name,
    and kind are unchanged from the previous captured schema, the old resolved
    type remains authoritative and is safe to carry forward.
    """
    if not base_schema_path:
        return 0
    base = json.loads(Path(base_schema_path).read_text())
    restored = 0
    for message_name, fields in messages.items():
        old_fields = {
            (field.get("no"), field.get("name"), field.get("kind")): field
            for field in base.get("messages", {}).get(message_name, [])
        }
        for field in fields:
            if field.get("kind") != "message" or field.get("T_name"):
                continue
            old = old_fields.get((field.get("no"), field.get("name"), field.get("kind")))
            if old and old.get("T_name"):
                field["T_name"] = old["T_name"]
                restored += 1
    return restored


def main():
    print(f"Loading {WB} ...", file=sys.stderr)
    js = WB.read_text()
    print(f"  {len(js) / 1e6:.1f} MB", file=sys.stderr)

    print("Building class ref map...", file=sys.stderr)
    ref_map = build_ref_map(js)
    print(f"  {len(ref_map)} refs", file=sys.stderr)

    print("Extracting messages...", file=sys.stderr)
    messages = extract_all_messages(js)
    print(f"  {len(messages)} messages", file=sys.stderr)

    print("Extracting enums...", file=sys.stderr)
    enums = extract_all_enums(js)
    print(f"  {len(enums)} enums", file=sys.stderr)

    unresolved = resolve_refs(messages, enums, ref_map)
    restored = inherit_resolved_refs(messages, BASE_SCHEMA)
    if restored:
        print(f"  Restored refs from base schema: {restored}", file=sys.stderr)
    print(f"  Unresolved refs: {len(unresolved)}", file=sys.stderr)
    if unresolved and len(unresolved) < 30:
        for r in list(unresolved)[:20]:
            print(f"    - {r}", file=sys.stderr)

    # Stats
    from collections import Counter
    ns = Counter()
    for name in messages.keys():
        parts = name.split('.')
        if len(parts) >= 2:
            ns[f'{parts[0]}.{parts[1]}'] += 1
    print("Namespaces (top 10):", file=sys.stderr)
    for k, v in sorted(ns.items(), key=lambda x: -x[1])[:10]:
        print(f"  {v:4d}  {k}", file=sys.stderr)

    out = {
        "cursor_version": CURSOR_VERSION,
        "cursor_commit": CURSOR_COMMIT,
        "class_refs": ref_map,
        "messages": messages,
        "enums": enums,
    }
    print(json.dumps(out, indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
