#!/bin/sh

set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/threadmill-install-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT
trap 'exit 1' HUP INT TERM

fake_bin="$test_root/fake-bin"
install_root="$test_root/install/.threadmill"
profile="$test_root/zshrc"
install_log="$test_root/install.log"
sudo_log="$test_root/sudo.log"
apparmor_ready="$test_root/apparmor-ready"
mkdir -p "$fake_bin"

for tool in awk bash cat chmod dirname git grep id mkdir mktemp rm sed sh uname; do
  ln -s "$(command -v "$tool")" "$fake_bin/$tool"
done

cat >"$fake_bin/go" <<'EOF'
#!/bin/sh
set -eu
case "${1:-}" in
  env)
    test "${2:-}" = GOVERSION
    printf 'go1.24.2\n'
    ;;
  install)
    mkdir -p "$GOBIN"
    cat >"$GOBIN/threadmill" <<'BINARY'
#!/bin/sh
printf 'threadmill smoke\n'
BINARY
    chmod 755 "$GOBIN/threadmill"
    ;;
  *)
    printf 'unexpected fake go command: %s\n' "$*" >&2
    exit 1
    ;;
esac
EOF
cat >"$fake_bin/bwrap" <<'EOF'
#!/bin/sh
if [ "${THREADMILL_TEST_REQUIRE_APPARMOR:-}" = 1 ] &&
  [ ! -e "$THREADMILL_TEST_APPARMOR_READY" ]; then
  exit 1
fi
exit 0
EOF
cat >"$fake_bin/sudo" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$THREADMILL_TEST_SUDO_LOG"
if [ "${1:-}" = -v ]; then
  exit 0
fi
exec "$@"
EOF
cat >"$fake_bin/sysctl" <<'EOF'
#!/bin/sh
test "$*" = "-n kernel.apparmor_restrict_unprivileged_userns"
printf '1\n'
EOF
cat >"$fake_bin/cp" <<'EOF'
#!/bin/sh
printf 'cp %s\n' "$*" >>"$THREADMILL_TEST_INSTALL_LOG"
EOF
cat >"$fake_bin/apparmor_parser" <<'EOF'
#!/bin/sh
printf 'apparmor_parser %s\n' "$*" >>"$THREADMILL_TEST_INSTALL_LOG"
: >"$THREADMILL_TEST_APPARMOR_READY"
EOF
cat >"$fake_bin/apt-get" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$THREADMILL_TEST_INSTALL_LOG"
case " $* " in
  *' install '*)
    for tool in fuse-overlayfs fusermount3; do
      cat >"$THREADMILL_TEST_BIN/$tool" <<'TOOL'
#!/bin/sh
exit 0
TOOL
      chmod 755 "$THREADMILL_TEST_BIN/$tool"
    done
    ;;
esac
EOF
chmod 755 "$fake_bin/go" "$fake_bin/bwrap" "$fake_bin/sudo" \
  "$fake_bin/sysctl" "$fake_bin/cp" "$fake_bin/apparmor_parser" "$fake_bin/apt-get"
printf '# existing profile\n' >"$profile"

run_installer() {
  PATH="$fake_bin" \
    SHELL=/bin/zsh \
    THREADMILL_TEST_BIN="$fake_bin" \
    THREADMILL_TEST_INSTALL_LOG="$install_log" \
    THREADMILL_TEST_SUDO_LOG="$sudo_log" \
    THREADMILL_TEST_REQUIRE_APPARMOR=1 \
    THREADMILL_TEST_APPARMOR_READY="$apparmor_ready" \
    THREADMILL_INSTALL_DIR="$install_root" \
    THREADMILL_PROFILE="$profile" \
    sh "$repo_root/scripts/install.sh"
}

run_installer
run_installer

test -x "$install_root/bin/threadmill"
test "$(grep -c '^# >>> Threadmill installer >>>$' "$profile")" -eq 1
grep -F "export PATH=\"$install_root/bin:\$PATH\"" "$profile" >/dev/null
test ! -e "$install_root/config.yaml"
test ! -e "$install_root/credentials.yaml"
test "$("$install_root/bin/threadmill")" = "threadmill smoke"
grep -F 'install -y fuse-overlayfs fuse3' "$install_log" >/dev/null
grep -F 'install -y apparmor-profiles' "$install_log" >/dev/null
grep -F 'cp /usr/share/apparmor/extra-profiles/bwrap-userns-restrict /etc/apparmor.d/bwrap-userns-restrict' "$install_log" >/dev/null
grep -F 'apparmor_parser -r /etc/apparmor.d/bwrap-userns-restrict' "$install_log" >/dev/null
grep -x -- '-v' "$sudo_log" >/dev/null
test -e "$apparmor_ready"
test -x "$fake_bin/fuse-overlayfs"
test -x "$fake_bin/fusermount3"

printf 'installer acceptance: PASS\n'
