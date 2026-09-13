#!/usr/bin/env python3

import argparse
import sys

import yaml


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("path")
    parser.add_argument("--reconciliation", required=True)
    parser.add_argument("--availability", required=True)
    parser.add_argument("--lifecycle", default="Active")
    args = parser.parse_args()

    with open(args.path, encoding="utf-8") as stream:
        documents = [document for document in yaml.safe_load_all(stream) if document]

    application = next(
        document for document in documents if document.get("kind") == "Application"
    )
    health = application["status"]["kubeHealth"]
    expected = {
        "reconciliation": args.reconciliation,
        "availability": args.availability,
        "lifecycle": args.lifecycle,
    }
    for dimension, status in expected.items():
        actual = health[dimension]["status"]
        if actual != status:
            raise AssertionError(f"{dimension} was {actual!r}, expected {status!r}")

    kinds = {document.get("kind") for document in documents}
    if "Deployment" not in kinds:
        raise AssertionError("render omitted the desired Deployment")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(error, file=sys.stderr)
        raise
