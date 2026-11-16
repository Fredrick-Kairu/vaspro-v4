"""Checks the Python snippet against VASPRO's golden vectors
(test/contract runs it in the official python image)."""

import base64
import json
import sys

from vaspro_webhook import InvalidSignature, InvalidTimestamp, verify


def outcome(v):
    try:
        verify(v["secret"], v["timestamp"], v["signature"], base64.b64decode(v["body_base64"]), now=v["now"])
        return "ok"
    except InvalidTimestamp:
        return "bad_timestamp"
    except InvalidSignature:
        return "bad_signature"
    except Exception as e:  # noqa: BLE001 - reported as a failure
        return f"error: {e!r}"


def main():
    with open(sys.argv[1], encoding="utf-8") as f:
        vectors = json.load(f)["vectors"]
    passed = 0
    for v in vectors:
        got = outcome(v)
        if got != v["expect"]:
            print(f"FAIL {v['name']}: got {got}, want {v['expect']}")
            continue
        passed += 1
    print(f"python: {passed}/{len(vectors)} vectors ok")
    sys.exit(0 if passed == len(vectors) else 1)


if __name__ == "__main__":
    main()
