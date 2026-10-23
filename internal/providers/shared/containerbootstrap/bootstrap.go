// Package containerbootstrap owns the bootstrap script that configures a
// container lease over SSH: it installs the tools needed by the Crabbox SSH
// contract, writes the authorized key, and starts sshd on the lease port.
//
// One implementation is shared by the local-container and containerd SSH-lease
// providers so the guest contract cannot drift between them.
package containerbootstrap

import core "github.com/openclaw/crabbox/internal/cli"

const DockerSigningKeyFingerprint = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"

const ImagePathRestoreBlock = `# crabbox managed image PATH
if [ -r "$HOME/.config/crabbox/image-path" ]; then
  PATH="$(/bin/cat "$HOME/.config/crabbox/image-path"; printf '\001')"
  PATH="${PATH%?}"
  export PATH
fi
# end crabbox managed image PATH
`

const InstallVerifiedAPTKeyringScript = `
install_verified_apt_keyring() {
  apt_key_url="$1"
  apt_key_target="$2"
  apt_key_expected_fingerprint="$3"
  install -m 0755 -d "$(dirname "$apt_key_target")"
  apt_key_tmp_dir="$(mktemp -d "${apt_key_target}.tmp.XXXXXX")"
  apt_key_download="$apt_key_tmp_dir/downloaded.asc"
  apt_key_home="$apt_key_tmp_dir/gnupg"
  apt_key_output="$apt_key_tmp_dir/keyring.gpg"
  install -m 0700 -d "$apt_key_home"
  if curl -fsSL "$apt_key_url" >"$apt_key_download" &&
    GNUPGHOME="$apt_key_home" gpg --batch --import "$apt_key_download" >/dev/null 2>&1; then
    apt_key_actual_fingerprint="$(
      GNUPGHOME="$apt_key_home" gpg --batch --with-colons --fingerprint "$apt_key_expected_fingerprint" 2>/dev/null |
        awk -F: '$1 == "fpr" { print $10; exit }' || true
    )"
    if [ "$apt_key_actual_fingerprint" = "$apt_key_expected_fingerprint" ] &&
      GNUPGHOME="$apt_key_home" gpg --batch --export "$apt_key_expected_fingerprint" >"$apt_key_output" &&
      [ -s "$apt_key_output" ]; then
      chmod 0644 "$apt_key_output"
      mv -f "$apt_key_output" "$apt_key_target"
      rm -rf "$apt_key_tmp_dir"
      return 0
    fi
  fi
  rm -rf "$apt_key_tmp_dir"
  return 1
}
`

const BrowserInstallScript = `
if [ "${CRABBOX_BROWSER:-0}" = "1" ] && command -v apt-get >/dev/null 2>&1; then
  # A version check alone accepts a browser that cannot launch (missing
  # libraries, no sandbox). Prove a headless render instead. The bootstrap runs
  # as root, and Chromium refuses to start as root with its sandbox enabled;
  # the relaxation is confined to this one-shot probe and never reaches the
  # lease browser wrapper, which keeps its sandbox.
  browser_can_run_headless() {
    probe_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-browser-probe.XXXXXX")"
    case "$(basename "$1")" in
      firefox*|iceweasel*)
        "$1" --headless --screenshot "$probe_dir/probe.png" about:blank >/dev/null 2>&1
        ;;
      *)
        "$1" --headless --disable-gpu --disable-dev-shm-usage --no-sandbox --dump-dom about:blank >/dev/null 2>&1
        ;;
    esac
    probe_status=$?
    rm -rf "$probe_dir"
    return $probe_status
  }
  has_working_browser() {
    for candidate in google-chrome chromium firefox-esr firefox; do
      if candidate_path="$(command -v "$candidate" 2>/dev/null)" && "$candidate_path" --version >/dev/null 2>&1 && browser_can_run_headless "$candidate_path"; then
        return 0
      fi
    done
    return 1
  }
  browser_install_failed() {
    echo "crabbox $1; use a prebuilt image with a working browser or a supported Debian image" >&2
    exit 127
  }
  if ! has_working_browser; then
    apt-get update
    ID=""
    if [ -r /etc/os-release ]; then . /etc/os-release; fi
    if [ "$ID" = ubuntu ]; then
      # Ubuntu's Firefox package is a Snap transition, not a container browser.
      if ! apt-get install -y --no-install-recommends ca-certificates curl gnupg; then
        browser_install_failed "Mozilla Firefox signing key tools unavailable"
      fi
      if ! install_verified_apt_keyring "https://packages.mozilla.org/apt/repo-signing-key.gpg" /etc/apt/keyrings/crabbox-mozilla.gpg "35BAA0B33E9EB396F59CA838C0BA5CE6DC6315A3"; then
        browser_install_failed "Mozilla Firefox signing key verification failed"
      fi
      install -m 0755 -d /etc/apt/sources.list.d /etc/apt/preferences.d
      arch="$(dpkg --print-architecture)"
      cat > /etc/apt/sources.list.d/crabbox-mozilla.sources <<MOZILLA
Types: deb
URIs: https://packages.mozilla.org/apt
Suites: mozilla
Components: main
Architectures: $arch
Signed-By: /etc/apt/keyrings/crabbox-mozilla.gpg
MOZILLA
      cat > /etc/apt/preferences.d/crabbox-mozilla <<'MOZILLA'
Package: firefox
Pin: origin packages.mozilla.org
Pin-Priority: 1000

Package: firefox
Pin: release o=Ubuntu
Pin-Priority: -1
MOZILLA
      chmod 0644 /etc/apt/sources.list.d/crabbox-mozilla.sources /etc/apt/preferences.d/crabbox-mozilla
      if ! apt-get update -o APT::Update::Error-Mode=any ||
        ! apt-get install -y --no-install-recommends firefox/mozilla ||
        ! has_working_browser; then
        browser_install_failed "Mozilla Firefox installation failed"
      fi
    else
      for browser_package in chromium firefox-esr firefox; do
        if has_working_browser; then break; fi
        if apt-cache show "$browser_package" >/dev/null 2>&1; then
          apt-get install -y --no-install-recommends "$browser_package" || true
        fi
      done
    fi
  fi
fi
`

const Tools = "/usr/sbin/sshd git rsync curl sudo python3"
const Packages = "openssh-server ca-certificates git rsync curl sudo python3"

var Script = `
set -eu
# The containerd provider mounts the public key at /crabbox/authorized_keys;
# the local-container provider passes it in the environment.
if [ -r /crabbox/authorized_keys ]; then
  CRABBOX_AUTHORIZED_KEY="$(cat /crabbox/authorized_keys)"
fi
: "${CRABBOX_AUTHORIZED_KEY:=}"
image_path="${PATH:-/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin}"
export DEBIAN_FRONTEND=noninteractive
user="${CRABBOX_SSH_USER:-crabbox}"
work_root="${CRABBOX_WORK_ROOT:-/work/crabbox}"
ssh_port="${CRABBOX_SSH_PORT:-2222}"
docker_signing_key_fingerprint="` + DockerSigningKeyFingerprint + `"
` + InstallVerifiedAPTKeyringScript + `
home_dir=""
if [ -r /etc/passwd ]; then
  while IFS=: read -r account _ _ _ _ account_home _; do
    if [ "$account" = "$user" ]; then
      home_dir="$account_home"
      break
    fi
  done < /etc/passwd
fi
if [ -z "$home_dir" ]; then
  if [ "$user" = root ]; then
    home_dir=/root
  else
    home_dir="/home/$user"
  fi
fi
container_paths_overlap() {
  left="${1%/}"
  right="${2%/}"
  [ -n "$left" ] || left=/
  [ -n "$right" ] || right=/
  [ "$left" = "$right" ] && return 0
  { [ "$left" = / ] || [ "$right" = / ]; } && return 0
  case "$left/" in "$right/"*) return 0 ;; esac
  case "$right/" in "$left/"*) return 0 ;; esac
  return 1
}
check_host_volume_path() {
  if ! command -v readlink >/dev/null 2>&1; then
    echo "crabbox host volume validation requires readlink" >&2
    exit 127
  fi
  resolve_container_path() {
    candidate="${1%/}"
    [ -n "$candidate" ] || candidate=/
    probe="$candidate"
    suffix=""
    while :; do
      resolved="$(readlink -f "$probe" 2>/dev/null || true)"
      if [ -n "$resolved" ]; then
        printf '%s%s\n' "${resolved%/}" "$suffix"
        return
      fi
      [ "$probe" != / ] || break
      suffix="/${probe##*/}$suffix"
      probe="${probe%/*}"
      [ -n "$probe" ] || probe=/
    done
    printf '%s\n' "$candidate"
  }
  host_path="$(resolve_container_path "$1")"
  for managed_path in "$work_root" "$home_dir" /bin /boot /dev /etc /home /lib /lib64 /proc /root /run /sbin /sys /tmp /usr /var; do
    managed_path="$(resolve_container_path "$managed_path")"
    if container_paths_overlap "$host_path" "$managed_path"; then
      echo "crabbox host volume target $host_path overlaps bootstrap-managed path $managed_path" >&2
      exit 2
    fi
  done
  env | sed -n 's/^CRABBOX_CACHE_VOLUME_PATH_[0-9][0-9]*=//p' | while IFS= read -r cache_path; do
    [ -n "$cache_path" ] || continue
    cache_path="$(resolve_container_path "$cache_path")"
    if container_paths_overlap "$host_path" "$cache_path"; then
      echo "crabbox host volume target $host_path overlaps bootstrap-managed cache path $cache_path" >&2
      exit 2
    fi
  done
}
env | sed -n 's/^CRABBOX_HOST_VOLUME_PATH_[0-9][0-9]*=//p' | while IFS= read -r host_path; do
  [ -n "$host_path" ] || continue
  check_host_volume_path "$host_path"
done
need_install=0
	for tool in ` + Tools + `; do
	  command -v "$tool" >/dev/null 2>&1 || need_install=1
	done
	if [ "$need_install" = "1" ] && command -v apt-get >/dev/null 2>&1; then
	  apt-get update
	  apt-get install -y --no-install-recommends ` + Packages + `
	fi
if [ "${CRABBOX_DESKTOP:-0}" = "1" ] && command -v apt-get >/dev/null 2>&1; then
  apt-get update
  if [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "xfce" ]; then
    if [ "${CRABBOX_DESKTOP_ENV:-xfce}" = "gnome" ]; then
      apt-get install -y --no-install-recommends labwc wayvnc swaybg librsvg2-common gnome-panel wlr-randr grim slurp wtype wl-clipboard dbus-user-session xwayland xdg-desktop-portal-wlr xdg-desktop-portal-gtk gnome-terminal nautilus gsettings-desktop-schemas adwaita-icon-theme fonts-dejavu-core fonts-liberation iproute2 openssl procps netcat-openbsd novnc websockify
    else
      apt-get install -y --no-install-recommends labwc wayvnc foot grim slurp wtype wl-clipboard wlr-randr dbus-user-session xwayland xdg-desktop-portal-wlr fonts-dejavu-core fonts-liberation iproute2 openssl procps netcat-openbsd novnc websockify
    fi
  else
    apt-get install -y --no-install-recommends tigervnc-standalone-server tigervnc-tools xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal xfconf xfce4-settings xauth dbus-x11 x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel fonts-dejavu-core fonts-liberation iproute2 openssl arc-theme procps netcat-openbsd novnc websockify
  fi
fi
` + BrowserInstallScript + `
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && ! command -v docker >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
  apt-get update
  install_docker_cli=0
  if command -v gpg >/dev/null 2>&1 || apt-get install -y --no-install-recommends gnupg; then
    if [ -r /etc/os-release ]; then
      . /etc/os-release
      case "${ID:-}" in
        debian|ubuntu)
          install -m 0755 -d /etc/apt/keyrings /etc/apt/sources.list.d
          if install_verified_apt_keyring "https://download.docker.com/linux/${ID}/gpg" /etc/apt/keyrings/docker.gpg "$docker_signing_key_fingerprint"; then
            codename="${VERSION_CODENAME:-}"
            if [ -n "$codename" ]; then
              arch="$(dpkg --print-architecture)"
              docker_source_tmp="$(mktemp /etc/apt/sources.list.d/docker.list.tmp.XXXXXX)"
              if printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/%s %s stable\n' "$arch" "$ID" "$codename" >"$docker_source_tmp" &&
                chmod 0644 "$docker_source_tmp" &&
                mv -f "$docker_source_tmp" /etc/apt/sources.list.d/docker.list; then
                rm -f /etc/apt/keyrings/docker.asc
                if apt-get update && apt-get install -y --no-install-recommends docker-ce-cli; then
                  install_docker_cli=1
                else
                  rm -f /etc/apt/sources.list.d/docker.list
                fi
              else
                rm -f "$docker_source_tmp"
              fi
            fi
          else
            echo "Docker APT signing key verification failed; falling back to distro Docker CLI" >&2
          fi
          ;;
      esac
    fi
  else
    echo "Docker APT signing key verification unavailable; falling back to distro Docker CLI" >&2
  fi
  if [ "$install_docker_cli" != "1" ]; then
    # docker-cli is the client-only package on newer distros; docker.io still
    # supplies it on releases that do not publish the split package.
    if ! apt-get install -y --no-install-recommends docker.io docker-cli; then
      if ! apt-get install -y --no-install-recommends docker.io; then
        echo "Docker-compatible socket requested but neither docker.io nor docker-cli is available from this distro; use a Debian/Ubuntu-compatible image or preinstall docker" >&2
        exit 127
      fi
      echo "docker-cli is not published by this distro; docker.io supplied the Docker CLI" >&2
    fi
  fi
fi
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && ! command -v docker >/dev/null 2>&1; then
  echo "Docker-compatible socket requested but docker CLI is not installed; use a Debian/Ubuntu-compatible image or preinstall docker" >&2
  exit 127
fi
if ! command -v /usr/sbin/sshd >/dev/null 2>&1; then
  echo "missing /usr/sbin/sshd; use a Debian/Ubuntu-compatible image or a prebuilt Crabbox runner image" >&2
  exit 127
fi
if [ -f /etc/ssh/sshd_config ]; then
  if grep -qE '^[#[:space:]]*UsePAM[[:space:]]+' /etc/ssh/sshd_config; then
    sed -i 's/^[#[:space:]]*UsePAM[[:space:]].*/UsePAM no/' /etc/ssh/sshd_config
  else
    printf '\nUsePAM no\n' >> /etc/ssh/sshd_config
  fi
  if grep -qE '^[#[:space:]]*PasswordAuthentication[[:space:]]+' /etc/ssh/sshd_config; then
    sed -i 's/^[#[:space:]]*PasswordAuthentication[[:space:]].*/PasswordAuthentication no/' /etc/ssh/sshd_config
  else
    printf '\nPasswordAuthentication no\n' >> /etc/ssh/sshd_config
  fi
fi
if ! id "$user" >/dev/null 2>&1; then
  useradd -m -d "$home_dir" -s /bin/bash "$user"
fi
passwd -d "$user" >/dev/null 2>&1 || true
home_dir="$(getent passwd "$user" | cut -d: -f6)"
if [ -z "$home_dir" ]; then
  home_dir="/home/$user"
fi
# Named cache mounts can create the home before useradd runs.
if [ "${CRABBOX_MANAGED_HOME:-0}" = "1" ]; then
  chown -R "$user" "$home_dir"
fi
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && [ -S /var/run/docker.sock ]; then
  socket_gid="$(stat -c '%g' /var/run/docker.sock 2>/dev/null || true)"
  if [ -n "$socket_gid" ]; then
    socket_group="$(getent group "$socket_gid" | cut -d: -f1 || true)"
    if [ -z "$socket_group" ]; then
      socket_group="crabbox-docker"
      groupadd -g "$socket_gid" "$socket_group" 2>/dev/null || socket_group=""
    fi
    if [ -n "$socket_group" ]; then
      usermod -aG "$socket_group" "$user" || true
    fi
  fi
fi
mkdir -p /run/sshd "$work_root" "$home_dir/.ssh" /var/cache/crabbox/pnpm /var/cache/crabbox/npm
printf '%s\n' "$CRABBOX_AUTHORIZED_KEY" > "$home_dir/.ssh/authorized_keys"
chmod 700 "$home_dir/.ssh"
chmod 600 "$home_dir/.ssh/authorized_keys"
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ]; then
  chown -R "$user" "$home_dir/.ssh"
else
  chown -R "$user" "$home_dir/.ssh" "$work_root"
fi
mkdir -p "$home_dir/.config/crabbox"
printf '%s' "$image_path" > "$home_dir/.config/crabbox/image-path"
chown "$user" "$home_dir/.config" "$home_dir/.config/crabbox" "$home_dir/.config/crabbox/image-path"
chmod 0700 "$home_dir/.config/crabbox"
chmod 0600 "$home_dir/.config/crabbox/image-path"
image_path_hook=/etc/profile.d/crabbox-image-path.sh
install -d -m 0755 /etc/profile.d
image_path_hook_tmp="$(mktemp "${image_path_hook}.tmp.XXXXXX")"
cat > "$image_path_hook_tmp" <<'CRABBOX_IMAGE_PATH_HOOK'
` + ImagePathRestoreBlock + `CRABBOX_IMAGE_PATH_HOOK
chown 0:0 "$image_path_hook_tmp"
chmod 0644 "$image_path_hook_tmp"
mv -f "$image_path_hook_tmp" "$image_path_hook"
login_profile=""
for candidate in "$home_dir/.bash_profile" "$home_dir/.bash_login" "$home_dir/.profile"; do
  if [ -e "$candidate" ]; then
    login_profile="$candidate"
    break
  fi
done
if [ -z "$login_profile" ]; then
  login_profile="$home_dir/.bash_profile"
  : > "$login_profile"
  chmod 0644 "$login_profile"
fi
python3 - "$login_profile" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
data = path.read_bytes()
block = rb'''` + ImagePathRestoreBlock + `'''

def find_line_sequence(content, sequence, offset=0):
    position = content.find(sequence, offset)
    while position >= 0:
        if position == 0 or content[position - 1:position] == b"\n":
            return position
        position = content.find(sequence, position + 1)
    return -1

while True:
    block_start = find_line_sequence(data, block)
    if block_start < 0:
        break
    block_end = block_start + len(block)
    suffix = data[block_end:]
    remove_from = block_start
    if not suffix and remove_from > 0 and data[remove_from - 1:remove_from] == b"\n":
        remove_from -= 1
    data = data[:remove_from] + suffix
if data:
    data += b"\n"
path.write_bytes(data + block)
PY
chown "$user" "$login_profile"
chown -R "$user" /var/cache/crabbox
env | sed -n 's/^CRABBOX_CACHE_VOLUME_PATH_[0-9][0-9]*=//p' | while IFS= read -r cache_path; do
  [ -n "$cache_path" ] || continue
  mkdir -p "$cache_path"
  chown -R "$user" "$cache_path"
done
if command -v sudo >/dev/null 2>&1; then
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$user" > /etc/sudoers.d/crabbox
  chmod 440 /etc/sudoers.d/crabbox
fi
if [ "${CRABBOX_DESKTOP:-0}" = "1" ]; then
  desktop_env="${CRABBOX_DESKTOP_ENV:-xfce}"
  case "$desktop_env" in
    xfce|wayland|gnome) ;;
    *) echo "CRABBOX_DESKTOP_ENV must be xfce, wayland, or gnome" >&2; exit 2 ;;
  esac
  install -d -m 0750 -o "$user" /var/lib/crabbox
  if [ ! -s /var/lib/crabbox/vnc.password ]; then
    (umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)
  fi
  if [ "$desktop_env" != "xfce" ]; then
    chown "$user" /var/lib/crabbox/vnc.password
    chmod 0600 /var/lib/crabbox/vnc.password
    runtime="/tmp/crabbox-runtime-$(id -u "$user")"
    install -d -m 0700 -o "$user" "$runtime" "$home_dir/.config" "$home_dir/.config/labwc" "$home_dir/.config/wayvnc"
    if [ "$desktop_env" = "gnome" ]; then
cat > "$home_dir/.config/labwc/autostart" <<'AUTOSTART'
wlr-randr --output HEADLESS-1 --custom-mode 1920x1080 >/tmp/crabbox-wlr-randr.log 2>&1 || true
for _ in $(seq 1 20); do
  [ -S /tmp/.X11-unix/X0 ] && break
  sleep 0.2
done
export XDG_CURRENT_DESKTOP=GNOME
export XDG_SESSION_DESKTOP=gnome
theme="$(cat "$HOME/.config/crabbox/desktop-theme" 2>/dev/null || printf dark)"
if [ "$theme" = light ]; then
  export GTK_THEME=Adwaita
  gsettings set org.gnome.desktop.interface color-scheme prefer-light >/dev/null 2>&1 || true
  gsettings set org.gnome.desktop.interface gtk-theme Adwaita >/dev/null 2>&1 || true
else
  export GTK_THEME=Adwaita-dark
  gsettings set org.gnome.desktop.interface color-scheme prefer-dark >/dev/null 2>&1 || true
  gsettings set org.gnome.desktop.interface gtk-theme Adwaita-dark >/dev/null 2>&1 || true
fi
export DISPLAY="${DISPLAY:-:0}"
export GDK_BACKEND=x11
export MOZ_ENABLE_WAYLAND=0
wallpaper_file="$HOME/.config/crabbox/desktop-background-$theme.svg"
if command -v swaybg >/dev/null 2>&1; then
  (
    if swaybg -i "$wallpaper_file" -m fill; then
      exit 0
    else
      status=$?
    fi
    [ "$status" -lt 128 ] || exit "$status"
    exec swaybg -c "#0d1117"
  ) </dev/null >/tmp/crabbox-swaybg.log 2>&1 &
fi
gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &
gnome-terminal -- bash -l >/tmp/crabbox-gnome-terminal.log 2>&1 &
nautilus --new-window "$HOME" >/tmp/crabbox-nautilus.log 2>&1 &
AUTOSTART
    else
cat > "$home_dir/.config/labwc/autostart" <<'AUTOSTART'
wlr-randr --output HEADLESS-1 --custom-mode 1920x1080 >/tmp/crabbox-wlr-randr.log 2>&1 || true
foot --title='Crabbox Desktop' >/tmp/crabbox-foot.log 2>&1 &
AUTOSTART
    fi
    chmod 0755 "$home_dir/.config/labwc/autostart"
    cat > "$home_dir/.config/wayvnc/config" <<'WAYVNC'
address=127.0.0.1
port=5900
enable_auth=false
xkb_layout=us
WAYVNC
    if [ "$desktop_env" = "gnome" ]; then
    cat >/usr/local/bin/crabbox-configure-desktop-theme <<'THEME'
` + core.GnomeDesktopThemeScript() + `THEME
    chmod 0755 /usr/local/bin/crabbox-configure-desktop-theme
    fi
    chown -R "$user" "$home_dir/.config"
    cat >/usr/local/bin/crabbox-start-desktop <<'DESKTOP'
#!/bin/sh
set -eu
user="${CRABBOX_SSH_USER:-crabbox}"
desktop_env="${CRABBOX_DESKTOP_ENV:-wayland}"
case "$desktop_env" in
  wayland|gnome) ;;
  *) echo "CRABBOX_DESKTOP_ENV must be wayland or gnome for Wayland startup" >&2; exit 2 ;;
esac
runtime="/tmp/crabbox-runtime-$(id -u "$user")"
install -d -m 0700 -o "$user" "$runtime"
if ! pgrep -u "$user" -x labwc >/dev/null 2>&1; then
  rm -f /var/lib/crabbox/display.env
  su "$user" -s /bin/sh -c "CRABBOX_DESKTOP_ENV='$desktop_env' XDG_RUNTIME_DIR='$runtime' WLR_BACKENDS=headless WLR_LIBINPUT_NO_DEVICES=1 WLR_RENDERER=pixman MOZ_ENABLE_WAYLAND=1 dbus-run-session labwc >/tmp/crabbox-labwc.log 2>&1 &"
fi
display=""
for _ in $(seq 1 30); do
  for socket in "$runtime"/wayland-*; do
    [ -S "$socket" ] || continue
    display="${socket##*/}"
    break 2
  done
  sleep 1
done
[ -n "$display" ] || { echo "wayland socket not ready" >&2; exit 1; }
cat >/var/lib/crabbox/desktop.env <<EOF
CRABBOX_DESKTOP_ENV=$desktop_env
XDG_RUNTIME_DIR=$runtime
WAYLAND_DISPLAY=$display
EOF
if [ "$desktop_env" = "gnome" ]; then
  printf 'DISPLAY=:0\n' >>/var/lib/crabbox/desktop.env
  printf 'GDK_BACKEND=x11\n' >>/var/lib/crabbox/desktop.env
  printf 'MOZ_ENABLE_WAYLAND=0\n' >>/var/lib/crabbox/desktop.env
fi
chown "$user" /var/lib/crabbox/desktop.env
chmod 0644 /var/lib/crabbox/desktop.env
if [ "$desktop_env" = "gnome" ]; then
  CRABBOX_DESKTOP_USER="$user" /usr/local/bin/crabbox-configure-desktop-theme
fi
if ! ss -ltn | grep -q '127.0.0.1:5900'; then
  home_dir="$(getent passwd "$user" | cut -d: -f6)"
  su "$user" -s /bin/sh -c "XDG_RUNTIME_DIR='$runtime' WAYLAND_DISPLAY='$display' wayvnc --config '$home_dir/.config/wayvnc/config' --render-cursor --max-fps=60 >/tmp/crabbox-wayvnc.log 2>&1 &"
fi
DESKTOP
    chmod 0755 /usr/local/bin/crabbox-start-desktop
    CRABBOX_SSH_USER="$user" /usr/local/bin/crabbox-start-desktop
  else
  head -c 8 /var/lib/crabbox/vnc.password | tigervncpasswd -f > /var/lib/crabbox/vnc.pass
  chown "$user" /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
  chmod 0600 /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
  printf 'CRABBOX_DESKTOP_ENV=xfce\nDISPLAY=:99\n' >/var/lib/crabbox/desktop.env
  chown "$user" /var/lib/crabbox/desktop.env
  chmod 0644 /var/lib/crabbox/desktop.env
  install -d -m 0755 /usr/local/lib/crabbox
  cat >/usr/local/lib/crabbox/xfce-session.sh <<'SESSION_ENV'
` + core.XfceSessionEnvironmentScript() + `SESSION_ENV
  chmod 0644 /usr/local/lib/crabbox/xfce-session.sh
  cat >/usr/local/bin/crabbox-configure-desktop-theme <<'THEME'
` + core.XfceDesktopThemeScript() + `THEME
  cat >/usr/local/bin/crabbox-desktop-session <<'SESSION'
` + core.XfceDesktopSessionScript() + `SESSION
  mkdir -p /etc/xdg/autostart
  cat >/etc/xdg/autostart/crabbox-desktop.desktop <<'AUTOSTART'
[Desktop Entry]
Type=Application
Name=Crabbox Desktop
Exec=/usr/local/bin/crabbox-desktop-session
OnlyShowIn=XFCE;
AUTOSTART
  chmod 0755 /usr/local/bin/crabbox-configure-desktop-theme /usr/local/bin/crabbox-desktop-session
  cat >/usr/local/bin/crabbox-start-desktop <<'DESKTOP'
#!/bin/sh
set -eu
user="${CRABBOX_SSH_USER:-crabbox}"
runtime="/tmp/crabbox-runtime-$user"
install -d -m 0700 -o "$user" "$runtime"
if ! pgrep -u "$user" -f 'Xtigervnc :99' >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "XDG_RUNTIME_DIR='$runtime' Xtigervnc :99 -geometry 1920x1080 -depth 24 -localhost yes -rfbport 5900 -SecurityTypes VncAuth -PasswordFile=/var/lib/crabbox/vnc.pass -AlwaysShared -AcceptSetDesktopSize -nolisten tcp -ac >/tmp/crabbox-xvfb.log 2>&1 &"
fi
sleep 1
if ! pgrep -u "$user" -x xfce4-session >/dev/null 2>&1; then
  env -u DISPLAY CRABBOX_DESKTOP_USER="$user" /usr/local/bin/crabbox-configure-desktop-theme "${1:-}"
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' dbus-launch startxfce4 >/tmp/crabbox-desktop.log 2>&1 &"
else
  runuser -u "$user" -- env DISPLAY=:99 /usr/local/bin/crabbox-desktop-session "${1:-}"
fi
DESKTOP
  chmod 0755 /usr/local/bin/crabbox-start-desktop
  CRABBOX_SSH_USER="$user" /usr/local/bin/crabbox-start-desktop
  fi
fi
if [ "${CRABBOX_BROWSER:-0}" = "1" ]; then
  browser_path=""
  for candidate in google-chrome chromium firefox-esr firefox; do
    if candidate_path="$(command -v "$candidate" 2>/dev/null)" && "$candidate_path" --version >/dev/null 2>&1; then
      browser_path="$candidate_path"
      break
    fi
  done
  if [ -z "$browser_path" ]; then
    echo "browser requested but no supported browser package is available for this image architecture" >&2
    exit 127
  fi
  browser_wrapper=/usr/local/bin/crabbox-browser
  case "$(basename "$browser_path")" in
    firefox*|iceweasel*)
      if [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=gnome$' /var/lib/crabbox/desktop.env; then
        printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export DISPLAY="${DISPLAY:-:0}"' "exec \"$browser_path\" --width 1500 --height 900 \"\$@\"" > "$browser_wrapper"
      elif [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=wayland$' /var/lib/crabbox/desktop.env; then
        printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY MOZ_ENABLE_WAYLAND=1' "exec \"$browser_path\" --width 1500 --height 900 \"\$@\"" > "$browser_wrapper"
      else
        printf '%s\n' '#!/bin/sh' "exec \"$browser_path\" --width 1500 --height 900 \"\$@\"" > "$browser_wrapper"
      fi
      ;;
    *)
      if [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=gnome$' /var/lib/crabbox/desktop.env; then
        printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export DISPLAY="${DISPLAY:-:0}"' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY' 'export GDK_BACKEND=x11 MOZ_ENABLE_WAYLAND=0' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'theme="$(cat "${CRABBOX_DESKTOP_THEME_FILE:-$HOME/.config/crabbox/desktop-theme}" 2>/dev/null || printf dark)"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' 'if [ "$theme" = light ]; then' "  exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --blink-settings=preferredColorScheme=1 --user-data-dir=\"\$profile\" --ozone-platform=x11 --window-size=1500,900 --window-position=80,80 \"\$@\"" 'fi' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2 --user-data-dir=\"\$profile\" --ozone-platform=x11 --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
      elif [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=wayland$' /var/lib/crabbox/desktop.env; then
        printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY' 'export MOZ_ENABLE_WAYLAND=1' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --user-data-dir=\"\$profile\" --ozone-platform=wayland --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
      else
        printf '%s\n' '#!/bin/sh' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --user-data-dir=\"\$profile\" --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
      fi
      ;;
  esac
  chmod 0755 "$browser_wrapper"
  install -d -m 0755 /var/lib/crabbox
  printf 'CHROME_BIN=%s\nBROWSER=%s\n' "$browser_wrapper" "$browser_wrapper" > /var/lib/crabbox/browser.env
  chown "$user" /var/lib/crabbox/browser.env
  chmod 0644 /var/lib/crabbox/browser.env
fi
if [ "${CRABBOX_INIT_MODE:-direct}" = "systemd" ] && [ -d /run/systemd/system ]; then
  # Under a systemd image the unit that runs this script owns the process
  # lifetime. Starting sshd and returning is the ending; replacing PID 1 here
  # would kill the image init and every unit it supervises.
  sshd_started=0
  if command -v systemctl >/dev/null 2>&1 &&
    { systemctl start ssh.service >/dev/null 2>&1 || systemctl start sshd.service >/dev/null 2>&1; }; then
    sshd_started=1
  fi
  if [ "$sshd_started" != "1" ]; then
    /usr/sbin/sshd -e -p "$ssh_port"
  fi
  exit 0
fi
exec /usr/sbin/sshd -D -e -p "$ssh_port"
`
