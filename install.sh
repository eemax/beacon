#!/usr/bin/env bash
# Install or update beacon on a Linux box with systemd. Run as root from a checkout:
#
#   git clone https://github.com/eemax/beacon /opt/beacon && /opt/beacon/install.sh
#
# Options:
#   --version vX.Y.Z      release to install (default: the latest release)
#   --binary PATH         install this binary instead of downloading one (e.g. go build)
#   --source NAME         this box's name in the hub (default: hostname; kept once set)
#   --tunnel-token FILE   also run a Cloudflare Tunnel with this token (from the hub's
#                         `lookout source add`), so the hub reaches beacon with no open port
#
# Idempotent: re-run to update. Never touches existing feeds, tokens or collectors.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION="" BINARY="" SOURCE="" TUNNEL_TOKEN=""
while (( $# )); do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --binary) BINARY="$2"; shift 2 ;;
    --source) SOURCE="$2"; shift 2 ;;
    --tunnel-token) TUNNEL_TOKEN="$2"; shift 2 ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "install: unknown option $1" >&2; exit 2 ;;
  esac
done

step() { printf '\n== %s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root"
command -v systemctl >/dev/null || die "needs systemd"

step "binary"
if [[ -z "$BINARY" ]]; then
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "no release build for $(uname -m); build one (go build ./cmd/beacon) and pass --binary" ;;
  esac
  if [[ -z "$VERSION" ]]; then
    VERSION=$(curl -fsSL https://api.github.com/repos/eemax/beacon/releases/latest \
      | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
    [[ -n "$VERSION" ]] || die "couldn't find the latest release; pass --version or --binary"
  fi
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  base="https://github.com/eemax/beacon/releases/download/$VERSION"
  curl -fsSL "$base/beacon-linux-$arch" -o "$tmp/beacon-linux-$arch"
  curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
  (cd "$tmp" && grep " beacon-linux-$arch\$" SHA256SUMS | sha256sum -c --quiet -) \
    || die "checksum mismatch for beacon-linux-$arch $VERSION"
  BINARY="$tmp/beacon-linux-$arch"
fi
before=$(sha256sum /usr/local/bin/beacon 2>/dev/null | cut -d' ' -f1 || true)
install -m 755 "$BINARY" /usr/local/bin/beacon
changed=0; [[ "$before" != "$(sha256sum /usr/local/bin/beacon | cut -d' ' -f1)" ]] && changed=1
/usr/local/bin/beacon version

step "user, folders, config"
id beacon &>/dev/null \
  || useradd --system --user-group --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin beacon
install -d -m 755 /var/lib/beacon /var/lib/beacon/feeds     # root writes feeds, beacon reads
install -d -m 750 -g beacon /etc/beacon
install -d -m 755 /etc/beacon/collectors.d                  # root's: collectors run as root
if [[ ! -f /etc/beacon/tokens ]]; then
  printf '# beacon tokens: <name> <sha256 of the token>. Manage with `beacon token`.\n' > /etc/beacon/tokens
fi
chown root:beacon /etc/beacon/tokens; chmod 640 /etc/beacon/tokens
if [[ ! -f /etc/beacon/beacon.env ]]; then
  cat > /etc/beacon/beacon.env <<EOF
# beacon settings (see \`beacon help\`). Read by the systemd units.
BEACON_SOURCE=${SOURCE:-$(hostname -s)}
#BEACON_LISTEN=127.0.0.1:7600
#BEACON_HOST_FEED=off
EOF
elif [[ -n "$SOURCE" ]]; then
  sed -i "s/^BEACON_SOURCE=.*/BEACON_SOURCE=$SOURCE/" /etc/beacon/beacon.env
fi
chown root:beacon /etc/beacon/beacon.env; chmod 640 /etc/beacon/beacon.env
grep '^BEACON_SOURCE=' /etc/beacon/beacon.env

step "units"
for unit in beacon.service beacon-collect.service beacon-collect.timer; do
  install -m 644 "$REPO/deploy/$unit" "/etc/systemd/system/$unit"
done
systemctl daemon-reload
systemctl enable --quiet --now beacon.service beacon-collect.timer
(( changed )) && systemctl restart beacon.service
systemctl start beacon-collect.service
systemctl is-active beacon.service

if [[ -n "$TUNNEL_TOKEN" ]]; then
  step "cloudflare tunnel"
  [[ -s "$TUNNEL_TOKEN" ]] || die "--tunnel-token: $TUNNEL_TOKEN is empty or missing"
  if ! command -v cloudflared >/dev/null; then
    command -v apt-get >/dev/null || die "install cloudflared (>= 2025.7) first: https://pkg.cloudflare.com"
    install -d -m 755 /usr/share/keyrings
    curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
    echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main" \
      > /etc/apt/sources.list.d/cloudflared.list
    apt-get update -q >/dev/null && DEBIAN_FRONTEND=noninteractive apt-get install -yq cloudflared >/dev/null
  fi
  cloudflared --version
  install -m 600 "$TUNNEL_TOKEN" /etc/beacon/tunnel.token
  install -m 644 "$REPO/deploy/beacon-tunnel.service" /etc/systemd/system/beacon-tunnel.service
  systemctl daemon-reload
  systemctl enable --quiet beacon-tunnel.service
  systemctl restart beacon-tunnel.service
  systemctl is-active beacon-tunnel.service
fi

step "status"
/usr/local/bin/beacon validate || true
echo
echo "Next: give the hub a token (shown once; pipe it straight to where it's needed):"
echo "  beacon token add lookout"
