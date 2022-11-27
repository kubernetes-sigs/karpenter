#!/usr/bin/env bash
set -euo pipefail

HEAD_HASH=$(git rev-parse HEAD)
if [ -z ${RELEASE_VERSION+x} ];then
  RELEASE_VERSION=${HEAD_HASH}
fi

echo "Releasing ${RELEASE_VERSION}, Commit: ${HEAD_HASH}"

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
source "${SCRIPT_DIR}/common.sh"

config
publishHelmChart "karpenter-core-crd" "${RELEASE_VERSION}"
