# Author: Patrick Munis at newwave
"""Load US state/county reference boundaries into the lakehouse ref zone.

Source: Census Bureau TIGER/Line cartographic boundary file, converted to
GeoJSON and MOUNTED into the analytics container at /ref/us_states.geojson
(operator task at deploy time — no runtime network dependency, so the geo
job is reproducible and air-gap safe). Expected properties per feature:
``state_code`` (2-letter USPS) and ``county_fips`` (or the TIGER
STATEFP/COUNTYFP pair, which this loader normalizes).

Output: s3a://lakehouse/ref/us_states parquet with columns
state_code, county_fips, geom_wkt — WKT keeps the ref zone engine-agnostic;
Sedona parses it with ST_GeomFromWKT.

Run: python load_us_states.py [path-to-geojson]
"""

from __future__ import annotations

import json
import os
import sys

import pyarrow as pa

from object_store import get_operator, write_parquet

DEFAULT_PATH = "/ref/us_states.geojson"


def _wkt(geom: dict) -> str:
    """GeoJSON geometry -> WKT (Polygon/MultiPolygon only — all Census
    county/state boundary files are)."""
    def ring(coords):
        return "(" + ", ".join(f"{x} {y}" for x, y in coords) + ")"

    def poly(rings):
        return "(" + ", ".join(ring(r) for r in rings) + ")"

    t = geom["type"]
    if t == "Polygon":
        return f"POLYGON {poly(geom['coordinates'])}"
    if t == "MultiPolygon":
        return "MULTIPOLYGON (" + ", ".join(poly(p) for p in geom["coordinates"]) + ")"
    raise ValueError(f"unsupported geometry type {t!r}")


def load(path: str = DEFAULT_PATH) -> int:
    with open(path) as f:
        gj = json.load(f)
    if gj.get("type") != "FeatureCollection":
        raise ValueError("expected a GeoJSON FeatureCollection")

    rows = {"state_code": [], "county_fips": [], "geom_wkt": []}
    for feat in gj["features"]:
        props = feat.get("properties") or {}
        state = props.get("state_code")
        county = props.get("county_fips")
        if not state and props.get("STATEFP"):  # raw TIGER attributes
            state = props["STATEFP"]
        if not county and props.get("COUNTYFP") and props.get("STATEFP"):
            county = props["STATEFP"] + props["COUNTYFP"]
        if not state:
            continue
        rows["state_code"].append(str(state).lower())
        rows["county_fips"].append(str(county or ""))
        rows["geom_wkt"].append(_wkt(feat["geometry"]))
    if not rows["state_code"]:
        raise ValueError(f"no usable features in {path}")

    table = pa.table(rows)
    out = "ref/us_states/us_states.parquet"  # relative to the opendal bucket
    write_parquet(get_operator(), out, table)
    print(f"load_us_states: {table.num_rows} boundaries -> {out}")
    return table.num_rows


if __name__ == "__main__":
    load(sys.argv[1] if len(sys.argv) > 1 else DEFAULT_PATH)
