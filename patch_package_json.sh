#!/bin/bash
jq '.scripts = {
    "dev": "next dev",
    "build": "next build",
    "start": "next start",
    "lint": "next lint",
    "test": "vitest run"
}' dashboard/package.json > dashboard/package.json.tmp && mv dashboard/package.json.tmp dashboard/package.json
