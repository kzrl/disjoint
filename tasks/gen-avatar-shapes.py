#!/usr/bin/env python3
"""Regenerate internal/avatars/shapes.go from the DiceBear "pixelbot" style.

Usage:
    python3 tasks/gen-avatar-shapes.py [path/to/src/pixelbot.json]

Defaults to the checked-in style at /home/k/src/dicebear; if the style is
installed as an npm package, point the argument at its src/pixelbot.json.
The output is deterministic: run it, review the diff, commit.
"""
import json
import re
import sys

DEFAULT_STYLE = "/tmp/dbstyles/package/src/pixelbot.json"


def pixel_starts(dstr):
    """Yield the absolute (x, y) top-left of every rounded-rect subpath.

    Each subpath is a rounded square: M start, h, a, v, a, h, a, v, a,
    which ends exactly back at its start, so a following relative 'm'
    offsets from the previous subpath's start.
    """
    pts = []
    tokens = re.findall(r"([Mmahvza])\s*([-\d.ea-z ]+?)(?=[Mmahvza]|$)", dstr)
    pos = (0.0, 0.0)
    start = (0.0, 0.0)
    for cmd, args in tokens:
        nums = (
            [float(x) for x in re.findall(r"-?\d*\.?\d+(?:e-?\d+)?", args)]
            if cmd != "z"
            else []
        )
        if cmd == "M":
            pos = start = (nums[0], nums[1])
            pts.append(pos)
        elif cmd == "m":
            pos = start = (start[0] + nums[0], start[1] + nums[1])
            pts.append(pos)
        elif cmd == "h":
            pos = (pos[0] + nums[0], pos[1])
        elif cmd == "v":
            pos = (pos[0], pos[1] + nums[0])
        elif cmd == "a":
            # 7 params; the endpoint is the last two, relative.
            pos = (pos[0] + nums[5], pos[1] + nums[6])
        elif cmd == "z":
            pos = start
    return pts


def r2(v):
    return round(v, 2)


def fmt(x):
    return ("%.2f" % x).rstrip("0").rstrip(".")


def coord_list(values):
    return "{ " + ", ".join(fmt(v) for v in values) + " }"


def comp_data(d, name, tx, ty):
    """Return (absXs, absYs, [(variantName, [rows], litCount)]).

    Pixels are collected in component-local coordinates and deduped per
    logical column/row; the component translate (tx, ty) is applied only
    to the emitted coordinate lists.
    """
    variants = []
    for vname, var in d["components"][name]["variants"].items():
        cells = set()
        for el in var["elements"]:
            cells |= {(r2(x), r2(y)) for x, y in pixel_starts(el["attributes"]["d"])}
        variants.append((vname, cells))
    allx = sorted({x for _, c in variants for x, _ in c})
    ally = sorted({y for _, c in variants for _, y in c})
    grids = []
    for vname, cells in variants:
        lines = ["".join("#" if (x, y) in cells else "." for x in allx) for y in ally]
        grids.append((vname, lines, sum(l.count("#") for l in lines)))
    return [r2(tx + x) for x in allx], [r2(ty + y) for y in ally], grids


def main():
    path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_STYLE
    d = json.load(open(path))

    bgpts = pixel_starts(d["canvas"]["elements"][0]["children"][0]["attributes"]["d"])
    dot_xs = sorted(set(r2(x) for x, _ in bgpts))
    dot_ys = sorted(set(r2(y) for _, y in bgpts))
    assert len(dot_xs) == 12 and len(dot_ys) == 12 and len(bgpts) == 144, (
        "dot matrix changed shape: %dx%d, %d dots" % (len(dot_xs), len(dot_ys), len(bgpts))
    )

    eyes_xs, eyes_ys, eyes_g = comp_data(d, "eyes", 17, 25)
    mouth_xs, mouth_ys, mouth_g = comp_data(d, "mouth", 17, 67)

    out = []
    out.append("// Code generated from the DiceBear \"pixelbot\" style definition")
    out.append("// (@dicebear/styles v10.6.0, src/pixelbot.json) by tasks/gen-avatar-shapes.py.")
    out.append("// DO NOT EDIT BY HAND.")
    out.append("package avatars")
    out.append("")
    out.append("// pixelSize and pixelRadius are the side and corner radius of one lit")
    out.append("// pixel (rounded square) in the 100x100 canvas coordinate space.")
    out.append("const pixelSize = 7.5")
    out.append("const pixelRadius = 1.9")
    out.append("")
    out.append("// dotXs and dotYs locate the 12x12 background dot matrix; every dot is")
    out.append("// drawn, in dark semi-transparent black.")
    out.append("var dotXs = [...]float64" + coord_list(dot_xs))
    out.append("var dotYs = [...]float64" + coord_list(dot_ys))
    out.append("")
    for part, xs, ys, grids in (
        ("eyes", eyes_xs, eyes_ys, eyes_g),
        ("mouth", mouth_xs, mouth_ys, mouth_g),
    ):
        out.append("var %sXs = [...]float64%s" % (part, coord_list(xs)))
        out.append("var %sYs = [...]float64%s" % (part, coord_list(ys)))
        out.append("")
        out.append(
            "// %sVariants are the %s shapes: %d rows of %d columns, \"#\" is a lit"
            % (part.capitalize(), part, len(ys), len(xs))
        )
        out.append("// pixel. Selection order matches the style definition.")
        out.append("var %sVariants = []string{" % part)
        for vname, lines, n in grids:
            out.append("\t// %s (%d px)" % (vname, n))
            out.append('\t"%s",' % "\\n".join(lines))
        out.append("}")
        out.append("")
    out.append("// backgrounds are the dark ground colors, and glows the lit pixel")
    out.append("// colors: one background and one glow per avatar.")
    out.append("var backgrounds = []string{")
    for c in d["colors"]["background"]["values"]:
        out.append('\t"%s",' % c)
    out.append("}")
    out.append("")
    out.append("var glows = []string{")
    for c in d["colors"]["glow"]["values"]:
        out.append('\t"%s",' % c)
    out.append("}")
    out.append("")

    open("/home/k/src/disjoint/internal/avatars/shapes.go", "w").write("\n".join(out))
    print("wrote internal/avatars/shapes.go")


if __name__ == "__main__":
    main()
