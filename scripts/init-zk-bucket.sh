#!/bin/sh
# init-zk-bucket.sh — create the `kmail-blobs` bucket in the local
# zk-object-fabric S3 gateway.
#
# `docker compose up` runs this indirectly via the `zk-fabric-init`
# one-shot service (see docker-compose.yml), which uses the
# amazon/aws-cli image with the kmail-dev tenant credentials. This
# script exists as a standalone helper for developers who want to
# re-create the bucket from the host — e.g. after wiping volumes
# with `docker compose down -v`.
#
# The gateway treats CreateBucket on an existing bucket as a no-op,
# so rerunning this is safe. Override ZK_FABRIC_S3_URL / credentials
# to point at a non-default compose network or a CI sandbox.
set -eu

: "${ZK_FABRIC_S3_URL:=http://localhost:9080}"
: "${AWS_ACCESS_KEY_ID:=kmail-access-key}"
: "${AWS_SECRET_ACCESS_KEY:=kmail-secret-key}"
: "${AWS_DEFAULT_REGION:=us-east-1}"
: "${KMAIL_BLOBS_BUCKET:=kmail-blobs}"

export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_DEFAULT_REGION

echo "init-zk-bucket: waiting for ${ZK_FABRIC_S3_URL} ..."
i=0
while ! wget -q --spider "${ZK_FABRIC_S3_URL}/" 2>/dev/null; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then
    echo "init-zk-bucket: gateway not reachable after 60s" >&2
    exit 1
  fi
  sleep 1
done

echo "init-zk-bucket: creating s3://${KMAIL_BLOBS_BUCKET}"
aws --endpoint-url "${ZK_FABRIC_S3_URL}" s3 mb "s3://${KMAIL_BLOBS_BUCKET}" || true
