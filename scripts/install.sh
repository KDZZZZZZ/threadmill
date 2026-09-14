#!/bin/sh

set -eu
umask 077

min_go_version=1.24.2
bootstrap_go_version=1.27.0
module_root=github.com/KDZZZZZZ/threadmill
module=$module_root/cmd/threadmill
repository=https://github.com/KDZZZZZZ/threadmill
ref=${THREADMILL_REF:-dev-native}
install_root=${THREADMILL_INSTALL_DIR:-"${HOME:?HOME is required}/.threadmill"}
bin_dir=$install_root/bin
bin_path=$bin_dir/threadmill
profile_override=${THREADMILL_PROFILE:-}
temp_dir=
project_probe=
state_probe=
path_probe=
toolchain_stage=

step() {
  printf '==> %s\n' "$1"
}

fail() {
  printf 'ERROR: %s\n' "$1" >&2
  exit 1
}

cleanup() {
  for probe in "$project_probe" "$state_probe" "$path_probe"; do
    [ -z "$probe" ] || rm -rf -- "$probe"
  done
  if [ -n "$temp_dir" ] && [ -d "$temp_dir" ]; then
    rm -rf "$temp_dir"
  fi
  if [ -n "$toolchain_stage" ] && [ -d "$toolchain_stage" ]; then
    rm -rf "$toolchain_stage"
  fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

case "$install_root" in
  /*) ;;
  *) fail "THREADMILL_INSTALL_DIR must be an absolute path" ;;
esac
if printf '%s' "$install_root" | LC_ALL=C grep '[[:cntrl:]]' >/dev/null 2>&1; then
  fail "THREADMILL_INSTALL_DIR must not contain control characters"
fi
case "$bin_dir" in
  *'`'* | *'$'* | *'"'* | *'\'*)
    fail "THREADMILL_INSTALL_DIR contains shell metacharacters that cannot be written safely to PATH"
    ;;
esac
case "$ref" in
  '' | *[!A-Za-z0-9._/-]*) fail "invalid THREADMILL_REF: $ref" ;;
esac

os=$(uname -s)
arch=$(uname -m)
if [ "$os" != Linux ]; then
  fail "the one-command installer currently supports Linux only"
fi
case "$arch" in
  x86_64 | amd64) go_arch=amd64 ;;
  aarch64 | arm64) go_arch=arm64 ;;
  *) fail "unsupported Linux architecture: $arch" ;;
esac

temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/threadmill-install.XXXXXX")

run_as_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    fail "installing runtime dependencies requires root or sudo"
  fi
}

require_admin_access() {
  [ "$(id -u)" -eq 0 ] && return 0
  command -v sudo >/dev/null 2>&1 || fail "installation requires root or sudo"
  step "Requesting administrator access for runtime setup"
  sudo -v || fail "administrator access is required to install Threadmill"
}

install_runtime_dependencies() {
  packages=
  command -v bash >/dev/null 2>&1 || packages="$packages bash"
  command -v bwrap >/dev/null 2>&1 || packages="$packages bubblewrap"
  command -v git >/dev/null 2>&1 || packages="$packages git"
  [ -n "$packages" ] || return 0

  step "Installing runtime dependencies:$packages"
  if command -v apt-get >/dev/null 2>&1; then
    run_as_root apt-get update
    # packages is a fixed list assembled above; intentional word splitting.
    run_as_root apt-get install -y $packages
  elif command -v dnf >/dev/null 2>&1; then
    run_as_root dnf install -y $packages
  elif command -v yum >/dev/null 2>&1; then
    run_as_root yum install -y $packages
  elif command -v apk >/dev/null 2>&1; then
    run_as_root apk add $packages
  elif command -v pacman >/dev/null 2>&1; then
    run_as_root pacman -Sy --needed --noconfirm $packages
  elif command -v zypper >/dev/null 2>&1; then
    run_as_root zypper --non-interactive install $packages
  else
    fail "install bash and bubblewrap with the system package manager, then rerun"
  fi

  command -v bash >/dev/null 2>&1 || fail "bash installation did not provide the bash command"
  command -v bwrap >/dev/null 2>&1 || fail "bubblewrap installation did not provide the bwrap command"
  command -v git >/dev/null 2>&1 || fail "git installation did not provide the git command"
}

download() {
  url=$1
  output=$2
  if command -v curl >/dev/null 2>&1; then
    curl --fail --silent --show-error --location "$url" --output "$output"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$output" "$url"
  else
    fail "curl or wget is required"
  fi
}

file_sha256() {
  path=$1
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$path" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$path" | sed 's/^.*= //'
  else
    fail "sha256sum, shasum, or openssl is required to verify the Go toolchain"
  fi
}

version_at_least() {
  awk -v have="$1" -v need="$2" 'BEGIN {
    split(have, h, ".")
    split(need, n, ".")
    for (i = 1; i <= 3; i++) {
      if ((h[i] + 0) > (n[i] + 0)) exit 0
      if ((h[i] + 0) < (n[i] + 0)) exit 1
    }
    exit 0
  }'
}

bootstrap_go() {
  command -v tar >/dev/null 2>&1 || fail "tar is required to install Go"
  case "$go_arch" in
    amd64) expected=675c26c449cbb18fc24b74650de1eabbae6e16f64326fd85a283fb3b58280685 ;;
    arm64) expected=51798d2c42d0e1c6ed7fd9f48728b4193abac9e8aad6dbac2fe96a81f5909bda ;;
  esac
  archive=$temp_dir/go.tar.gz
  go_root=$install_root/toolchains/go$bootstrap_go_version

  # Official archive names and SHA-256 values:
  # https://go.dev/dl/?mode=json&include=all
  step "Installing private Go $bootstrap_go_version toolchain"
  download "https://go.dev/dl/go$bootstrap_go_version.linux-$go_arch.tar.gz" "$archive"
  actual=$(file_sha256 "$archive")
  [ "$actual" = "$expected" ] || fail "Go toolchain checksum mismatch"
  mkdir -p "$install_root/toolchains"
  toolchain_stage=$(mktemp -d "$install_root/toolchains/.go$bootstrap_go_version.XXXXXX")
  tar -C "$toolchain_stage" --strip-components=1 -xzf "$archive"
  [ -x "$toolchain_stage/bin/go" ] || fail "downloaded Go toolchain is incomplete"
  if [ -e "$go_root" ]; then
    fail "existing private Go toolchain is incomplete: $go_root"
  fi
  mv "$toolchain_stage" "$go_root"
  toolchain_stage=
  go_bin=$go_root/bin/go
}

select_go() {
  if command -v go >/dev/null 2>&1; then
    candidate=$(command -v go)
    version=$($candidate env GOVERSION 2>/dev/null || true)
    version=${version#go}
    if version_at_least "$version" "$min_go_version"; then
      go_bin=$candidate
      return
    fi
  fi

  private_go=$install_root/toolchains/go$bootstrap_go_version/bin/go
  if [ -x "$private_go" ]; then
    go_bin=$private_go
    return
  fi
  bootstrap_go
}

# Probe the same user/PID namespaces and writable mounts used by the runner.
# https://github.com/containers/bubblewrap/blob/main/README.md
probe_bwrap() {
  probe_root=$state_probe/sandbox
  mkdir -p "$probe_root/tmp"
  for workspace in "$state_probe/clone" "$project_probe"; do
    bwrap \
      --unshare-user \
      --unshare-pid \
      --die-with-parent \
      --tmpfs / \
      --bind "$probe_root/tmp" /tmp \
      --setenv PATH /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
      --ro-bind-try /usr /usr \
      --ro-bind-try /bin /bin \
      --ro-bind-try /lib /lib \
      --ro-bind-try /lib64 /lib64 \
      --dev /dev \
      --proc /proc \
      --bind "$workspace" /workspace \
      --chdir /workspace \
      -- bash -c 'set -eu; ./executable; printf sandbox > written; mv written renamed; rm renamed' \
      >"$temp_dir/bwrap.log" 2>&1 || return 1
  done
}

prepare_bwrap() {
  if probe_bwrap; then
    step "Sandbox dependency ready: bwrap"
    return
  fi

  if [ "$(sysctl -n kernel.apparmor_restrict_unprivileged_userns 2>/dev/null || true)" = 1 ]; then
    command -v apt-get >/dev/null 2>&1 ||
      fail "AppArmor blocks bwrap; install and enable the distribution bwrap profile"
    step "Enabling the distribution AppArmor profile for bwrap"
    run_as_root apt-get update
    run_as_root apt-get install -y apparmor-profiles
    run_as_root cp \
      /usr/share/apparmor/extra-profiles/bwrap-userns-restrict \
      /etc/apparmor.d/bwrap-userns-restrict
    run_as_root apparmor_parser -r /etc/apparmor.d/bwrap-userns-restrict
  fi

  probe_bwrap || {
    cat "$temp_dir/bwrap.log" >&2
    fail "bwrap cannot create the required user namespace; refusing to install a command runner that cannot execute commands"
  }
  step "Sandbox dependency ready: bwrap"
}

pick_profile() {
  if [ -n "$profile_override" ]; then
    printf '%s\n' "$profile_override"
    return
  fi
  # Linux shell startup behavior:
  # https://www.gnu.org/software/bash/manual/html_node/Bash-Startup-Files
  case "${SHELL:-}" in
    */zsh) printf '%s\n' "$HOME/.zshrc" ;;
    */bash) printf '%s\n' "$HOME/.bashrc" ;;
    *) printf '%s\n' "$HOME/.profile" ;;
  esac
}

configure_path() {
  path_action=already
  case ":${PATH:-}:" in
    *":$bin_dir:"*) path_profile=; return ;;
  esac
  path_profile=$(pick_profile)
  marker='# >>> Threadmill installer >>>'
  if [ -f "$path_profile" ] && grep -F "$marker" "$path_profile" >/dev/null 2>&1; then
    path_action=configured
    return
  fi
  mkdir -p "$(dirname -- "$path_profile")"
  {
    printf '\n%s\n' "$marker"
    printf 'export PATH="%s:$PATH"\n' "$bin_dir"
    printf '# <<< Threadmill installer <<<\n'
  } >>"$path_profile"
  path_action=added
}

# This runs before package/toolchain/application installation. sudo is only for
# dependency setup: filesystem probes must run as the eventual application user.
probe_paths() {
  [ -z "${SUDO_USER:-}" ] || fail "run the installer as the intended Threadmill user, without sudo; it requests administrator access itself"
  command -v cp >/dev/null 2>&1 || fail "GNU cp with --reflink=always is required"
  project_dir=$(CDPATH= cd -- "${THREADMILL_PROJECT_DIR:-.}" && pwd -P) ||
    fail "select an existing project with THREADMILL_PROJECT_DIR"
  printf '%s' "$project_dir" >"$temp_dir/project-path"
  project_id=$(file_sha256 "$temp_dir/project-path")
  state_root="$HOME/.threadmill/projects/$project_id/vfs"
  case "$state_root/" in
    "$project_dir/"*) fail "project must not contain its Threadmill state directory; run the installer from your project directory" ;;
  esac
  step "Checking filesystem permissions and mandatory reflink for $project_dir"
  mkdir -p "$state_root" "$bin_dir" || fail "state and installation directories must be writable by the current user"
  state_probe=$(mktemp -d "$state_root/.preflight.XXXXXX") || fail "VFS state directory is not writable"
  project_probe=$(mktemp -d "$project_dir/.threadmill-preflight.XXXXXX") || fail "project directory is not writable"
  printf '#!/bin/sh\nexit 0\n' >"$project_probe/executable"
  chmod 700 "$project_probe/executable"
  "$project_probe/executable" || fail "project filesystem does not permit execution"
  ln -s executable "$project_probe/link" || fail "project filesystem must support symbolic links"
  mkdir "$state_probe/clone"
  # A nonempty file proves CoW support even when the project starts empty.
  # --reflink=always must fail rather than silently copy on unsupported filesystems.
  cp --reflink=always -a -- "$project_probe/." "$state_probe/clone" ||
    fail "reflink from project to $state_root is required; ordinary copying is disabled (use the same reflink-capable filesystem)"
  [ "$(readlink "$state_probe/clone/link")" = executable ] || fail "VFS clone did not preserve symbolic links"
  "$state_probe/clone/executable" || fail "VFS state filesystem does not permit execution"
  # Detect unreadable files and nested mounts before installing anything.
  mkdir "$state_probe/floor"
  cp --reflink=always -a -- "$project_dir/." "$state_probe/floor" ||
    fail "all project files must be readable and reflink-cloneable into VFS storage"
  for writable_dir in "$bin_dir" "$install_root/toolchains" "$install_root/cache/mod" "$install_root/cache/build" "$install_root/go" "$(dirname -- "$project_dir")" "$(dirname -- "$(pick_profile)")"; do
    mkdir -p "$writable_dir" || fail "required directory cannot be created: $writable_dir"
    path_probe=$(mktemp -d "$writable_dir/.threadmill-preflight.XXXXXX") ||
      fail "required directory is not writable: $writable_dir"
    printf '#!/bin/sh\nexit 0\n' >"$path_probe/executable"
    chmod 700 "$path_probe/executable"
    case "$writable_dir" in
      "$bin_dir" | "$install_root/toolchains" | "$(dirname -- "$project_dir")")
        "$path_probe/executable" || fail "required directory does not permit execution: $writable_dir"
        ;;
    esac
    mv "$path_probe/executable" "$path_probe/renamed" || fail "required directory does not permit rename: $writable_dir"
    rm -rf -- "$path_probe"
    path_probe=
  done
  profile_path=$(pick_profile)
  if [ -e "$profile_path" ]; then
    [ -f "$profile_path" ] && [ -w "$profile_path" ] || fail "shell profile is not a writable regular file: $profile_path"
  fi
  if [ -n "${GRADLE_USER_HOME:-}" ] && [ -d "$GRADLE_USER_HOME" ]; then
    mkdir "$state_probe/gradle"
    cp --reflink=always -a -- "$GRADLE_USER_HOME/." "$state_probe/gradle" ||
      fail "GRADLE_USER_HOME must be reflink-cloneable into VFS storage"
  fi
}

probe_paths
require_admin_access
install_runtime_dependencies
prepare_bwrap
select_go

install_ref=$ref
if remote_ref=$(git ls-remote --exit-code "$repository" "refs/heads/$ref"); then
  install_ref=$(printf '%s\n' "$remote_ref" | awk 'NR == 1 { print $1 }')
  [ -n "$install_ref" ] || fail "the requested branch resolved to an empty commit"
else
  status=$?
  [ "$status" -eq 2 ] || fail "cannot resolve Threadmill ref $ref"
fi

step "Installing Threadmill $ref to $bin_path"
mkdir -p "$bin_dir" "$install_root/cache/mod" "$install_root/cache/build" "$install_root/go"
chmod 700 "$install_root"
GOBIN="$bin_dir" \
  GOPATH="$install_root/go" \
  GOMODCACHE="$install_root/cache/mod" \
  GOCACHE="$install_root/cache/build" \
  GONOPROXY="$module_root" \
  GONOSUMDB="$module_root" \
  GOTOOLCHAIN=auto \
  CGO_ENABLED=0 \
  "$go_bin" install "$module@$install_ref"
chmod 755 "$bin_path"
"$bin_path" -h >/dev/null 2>&1 || fail "installed Threadmill binary failed its smoke test"

configure_path

step "VFS ready: mandatory reflink verified; ordinary copy fallback disabled"

step "Threadmill installed"
if [ "${path_action:-already}" = added ]; then
  step "PATH added to $path_profile; open a new terminal"
  step "Current terminal: export PATH=\"$bin_dir:\$PATH\""
elif [ "${path_action:-already}" = configured ]; then
  step "PATH is already configured in $path_profile"
fi
step "Run threadmill; the first TUI launch will ask for model settings and an API key"
