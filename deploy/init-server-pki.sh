#!/usr/bin/env bash
# Prepare PKI for the shipped server-*.json templates. Requires OpenSSL 1.1.1+.
set -euo pipefail
umask 077
command -v openssl >/dev/null || { echo 'OpenSSL 1.1.1+ is required.' >&2; exit 1; }
output_dir=config
if [[ $# -gt 0 ]]; then output_dir=$1; fi
if [[ -e "$output_dir" ]]; then
  echo "Output directory already exists: $output_dir. Refusing to replace existing certificates." >&2
  exit 1
fi
read -r -p 'Server IPv4 address (used by Agents and Web browsers): ' server_ip
read -r -p 'Server DNS name (optional, Enter to skip): ' server_dns
IFS=. read -r a b c d extra <<< "$server_ip"
if [[ ! "$server_ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ || -n "$extra" ]]; then
  echo 'Enter an IPv4 address, for example 192.0.2.10.' >&2
  exit 1
fi
for octet in "$a" "$b" "$c" "$d"; do
  if [[ ! "$octet" =~ ^[0-9]{1,3}$ ]] || (( 10#$octet > 255 )); then echo 'Invalid IPv4 address.' >&2; exit 1; fi
done
if [[ -n "$server_dns" && ! "$server_dns" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]]; then
  echo 'Invalid DNS name.' >&2
  exit 1
fi
san="IP:$server_ip,IP:127.0.0.1,DNS:localhost"
if [[ -n "$server_dns" ]]; then san+=",DNS:$server_dns"; fi
mkdir -m 0700 -- "$output_dir"
stage=$(mktemp -d)
if command -v cygpath >/dev/null; then
  # Git Bash otherwise rewrites /CN=... as a Windows path before invoking OpenSSL.
  output_dir=$(cygpath -m "$output_dir")
  stage=$(cygpath -m "$stage")
  export MSYS_NO_PATHCONV=1
fi
trap 'rm -rf -- "$stage"' EXIT
openssl req -x509 -newkey rsa:3072 -nodes -sha256 -days 3650 \
  -subj '/CN=Monitor Server CA' -keyout "$output_dir/server-ca.key" -out "$output_dir/server-ca.crt" \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -addext 'subjectKeyIdentifier=hash'
openssl req -x509 -newkey rsa:3072 -nodes -sha256 -days 3650 \
  -subj '/CN=Monitor Agent Issuing CA' -keyout "$output_dir/agent-ca.key" -out "$output_dir/agent-ca.crt" \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -addext 'extendedKeyUsage=clientAuth' \
  -addext 'subjectKeyIdentifier=hash'
cat > "$stage/server.ext" <<EXT
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid,issuer
subjectAltName=$san
EXT
for name in server web; do
  openssl req -new -newkey rsa:2048 -nodes -sha256 -subj "/CN=monitor-$name" \
    -keyout "$output_dir/$name.key" -out "$stage/$name.csr"
  openssl x509 -req -in "$stage/$name.csr" -CA "$output_dir/server-ca.crt" \
    -CAkey "$output_dir/server-ca.key" -CAcreateserial -sha256 -days 365 \
    -extfile "$stage/server.ext" -out "$output_dir/$name.crt"
done
openssl verify -x509_strict -CAfile "$output_dir/server-ca.crt" -purpose sslserver \
  "$output_dir/server.crt" "$output_dir/web.crt"
chmod 0644 "$output_dir"/*.crt
chmod 0600 "$output_dir"/*.key
echo
echo "Created PKI in $output_dir."
echo 'Use configs/server-sqlite.json or configs/server-postgres.json from the package root.'
echo 'Set web-password (or MONITOR_WEB_PASSWORD) and your database settings before starting.'
echo 'Server trusts agent-ca.crt; Web issuance uses agent-ca.crt/key; Agent ZIP contains server-ca.crt.'
echo 'Archive server-ca.key offline after setup; the running Server needs agent-ca.key for issuance.'
echo 'No certificates were installed into an existing service. Never rerun this to add an Agent.'
