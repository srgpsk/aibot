#!/usr/bin/env sh
set -e

BUILD_ARGS=""
while read -r line; do
 ARG_NAME="${line%=*}"
 ARG_VAL="${line#*=}"
 BUILD_ARGS=$BUILD_ARGS"--build-arg ${ARG_NAME}=${ARG_VAL} "
done < .env

. ./.env
docker build ${BUILD_ARGS}  --no-cache -t aibot-prod -f Dockerfile.production .