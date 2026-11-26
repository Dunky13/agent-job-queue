#!/bin/bash
# Source this file. sparkle_public_key_plist KEY_FILE prints the Info.plist entry for Sparkle's public EdDSA key.
# A published build (BUILD_NUMBER set) must have the key; a local build may omit it and prints nothing.
sparkle_public_key_plist() {
  local file=$1 key
  if [[ -f $file ]]; then
    key=$(< "$file")
    # One base64 line holding a 32-byte Ed25519 public key.
    [[ $key =~ ^[A-Za-z0-9+/]{43}=$ && $(printf '%s' "$key" | base64 -D | wc -c) -eq 32 ]] || {
      echo 'packaging/sparkle/public-ed-key.txt must hold one base64 line with a 32-byte Ed25519 public key.' >&2; return 1; }
    printf '<key>SUPublicEDKey</key><string>%s</string>' "$key"
  elif [[ -n ${BUILD_NUMBER:-} ]]; then
    echo 'A published build needs packaging/sparkle/public-ed-key.txt, the public EdDSA key for Sparkle updates.' >&2; return 1
  fi
}
