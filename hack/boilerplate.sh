#!/bin/bash
set -eu -o pipefail

# Ignore vendor downstream since upstream does not vendor.
for i in $(
  find ./ -name "*.go" -not -path "./vendor/*"
); do
  if ! grep -q "Apache License" $i; then
    cat hack/boilerplate.go.txt $i >$i.new && mv $i.new $i
  fi
done
