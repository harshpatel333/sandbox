#!/usr/bin/env python3
"""Render a Terraform template file outside Terraform.

The devbox runs the same host bootstrap templates Terraform hands to fleet
hosts, so it renders them with the subset of the template language those
files use: `${name}` interpolation, `$${`/`%%{` escapes, `if name != ""` /
`if name == ""` conditionals and `for` loops over lists and maps, with `~`
whitespace stripping. Anything else is an error rather than a guess, so a
template that grows new syntax fails here instead of rendering wrongly.

Usage: render_tftpl.py TEMPLATE VARS_JSON  (VARS_JSON may be - for stdin)
"""

import json
import re
import sys

TOKEN = re.compile(r"\$\$\{|%%\{|\$\{(~?)([^}]*?)(~?)\}|%\{(~?)([^}]*?)(~?)\}")
IDENT = r"[A-Za-z_][A-Za-z0-9_]*"
IF = re.compile(rf"if ({IDENT}) (==|!=) \"\"")
FOR = re.compile(rf"for (?:({IDENT}), )?({IDENT}) in ({IDENT})")


class TemplateError(ValueError):
    pass


def tokenize(text):
    out = []
    pos = 0
    for m in TOKEN.finditer(text):
        out.append(("text", text[pos:m.start()]))
        pos = m.end()
        raw = m.group(0)
        if raw == "$${":
            out.append(("text", "${"))
        elif raw == "%%{":
            out.append(("text", "%{"))
        elif raw.startswith("${"):
            out.append(("interp", m.group(2).strip(), bool(m.group(1)), bool(m.group(3))))
        else:
            out.append(("directive", m.group(5).strip(), bool(m.group(4)), bool(m.group(6))))
    out.append(("text", text[pos:]))
    for i, tok in enumerate(out):
        if tok[0] == "text":
            continue
        if tok[2]:
            raise TemplateError("leading ~ strip markers are not supported")
        if tok[3] and out[i + 1][0] == "text":
            out[i + 1] = ("text", re.sub(r"\A[ \t]*\r?\n?", "", out[i + 1][1]))
    return [tok[:2] for tok in out]


def parse(tokens, pos=0, closers=()):
    nodes = []
    while pos < len(tokens):
        kind, value = tokens[pos]
        if kind == "directive" and value in closers:
            return nodes, pos
        pos += 1
        if kind != "directive":
            nodes.append((kind, value))
            continue
        if m := IF.fullmatch(value):
            body, pos = parse(tokens, pos, ("else", "endif"))
            other = []
            if pos < len(tokens) and tokens[pos][1] == "else":
                other, pos = parse(tokens, pos + 1, ("endif",))
            if pos >= len(tokens):
                raise TemplateError(f"unterminated %{{ {value} }}")
            nodes.append(("if", m.group(1), m.group(2), body, other))
            pos += 1
        elif m := FOR.fullmatch(value):
            body, pos = parse(tokens, pos, ("endfor",))
            if pos >= len(tokens):
                raise TemplateError(f"unterminated %{{ {value} }}")
            nodes.append(("for", m.group(1), m.group(2), m.group(3), body))
            pos += 1
        else:
            raise TemplateError(f"unsupported directive %{{ {value} }}")
    if closers:
        raise TemplateError(f"missing %{{ {closers[-1]} }}")
    return nodes, pos


def lookup(scope, name):
    if not re.fullmatch(IDENT, name):
        raise TemplateError(f"unsupported expression ${{{name}}}")
    if name not in scope:
        raise TemplateError(f"undefined template variable {name}")
    return scope[name]


def render_nodes(nodes, scope):
    out = []
    for node in nodes:
        kind = node[0]
        if kind == "text":
            out.append(node[1])
        elif kind == "interp":
            value = lookup(scope, node[1])
            if not isinstance(value, str):
                raise TemplateError(f"{node[1]} must be a string to interpolate")
            out.append(value)
        elif kind == "if":
            _, name, op, body, other = node
            value = lookup(scope, name)
            if not isinstance(value, str):
                raise TemplateError(f"{name} must be a string to compare")
            out.append(render_nodes(body if (value == "") == (op == "==") else other, scope))
        else:
            _, key_name, value_name, collection, body = node
            items = lookup(scope, collection)
            if isinstance(items, dict):
                pairs = sorted(items.items())
            elif isinstance(items, list) and key_name is None:
                pairs = list(enumerate(items))
            else:
                raise TemplateError(f"cannot iterate {collection}")
            for key, value in pairs:
                inner = dict(scope, **{value_name: value})
                if key_name:
                    inner[key_name] = key
                out.append(render_nodes(body, inner))
    return "".join(out)


def render(text, variables):
    nodes, _ = parse(tokenize(text))
    return render_nodes(nodes, variables)


def main(argv):
    if len(argv) != 3:
        sys.exit(__doc__.strip().splitlines()[-1])
    with open(argv[1]) as f:
        text = f.read()
    raw = sys.stdin.read() if argv[2] == "-" else open(argv[2]).read()
    try:
        sys.stdout.write(render(text, json.loads(raw)))
    except TemplateError as e:
        sys.exit(f"{argv[1]}: {e}")


if __name__ == "__main__":
    main(sys.argv)
