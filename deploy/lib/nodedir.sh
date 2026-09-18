#!/usr/bin/env bash
# A directory on the node that a hostPath volume may be pointed at -- for
# deploy/weights.sh and hack/benchmark/model_hostpath.sh, which both hand a
# node directory to a hostPath PersistentVolume that a namespace's pods then
# mount read-write, as root.
#
# `type: DirectoryOrCreate` on an existing path bind-mounts it as it is, so a
# slip such as --path /var/lib/kubelet would hand every pod in the namespace
# root write on every other pod's secret volumes on every accelerator node.
# The caller holds PersistentVolume create rights, so this is a guard against
# a mistake, not an adversary -- but the mistake is one character away.

# nodedir_ok says whether $1 is an acceptable node directory: absolute, of
# path characters, at least two components (/mnt/local/models, not /mnt), and
# not under a prefix the node itself lives in. Prints why not, returns 1.
nodedir_ok() {
    local dir="$1"
    case "$dir" in
        "") echo "must not be empty"; return 1 ;;
        /) echo "must not be the root directory"; return 1 ;;
        *[!A-Za-z0-9._/-]*) echo "not a node path: '${dir}' (absolute, letters, digits, . _ - /)"; return 1 ;;
        /*) ;;
        *) echo "must be absolute: '${dir}'"; return 1 ;;
    esac
    case "$dir" in
        *..*) echo "must not contain '..': '${dir}'"; return 1 ;;
        */) echo "must not end with '/': '${dir}'"; return 1 ;;
    esac
    case "${dir#/}" in
        */*) ;;
        *) echo "needs at least two components (/mnt/local/models, not ${dir}): the whole of a top-level directory is not a weights directory"; return 1 ;;
    esac
    # /var/mnt and /var/srv themselves are RHCOS's root disk (the one the
    # image store and the eviction thresholds live on); a subdirectory with
    # a disk mounted at it is what the docs ask for
    case "$dir" in
        /var/mnt|/var/srv) echo "'${dir}' is the root disk on RHCOS; a subdirectory with a disk mounted at it (/var/mnt/weights)"; return 1 ;;
    esac
    local prefix
    # the second row is where RHCOS keeps /home, /root, /usr/local, /opt and
    # the ostree deployments (its /home, /root, /opt, /mnt are symlinks into /var)
    for prefix in /etc /usr /bin /sbin /lib /lib32 /lib64 /boot /dev /proc /sys /run /var/run /var/lib /var/log /var/spool /root /home /opt/cni /opt/bin /snap /tmp /var/tmp \
                  /var/home /var/roothome /var/usrlocal /var/opt/cni /var/opt/bin /sysroot /ostree; do
        case "$dir" in
            "$prefix"|"$prefix"/*) echo "'${dir}' is under ${prefix}, which the node itself lives in; a hostPath there is mounted read-write by every pod in the namespace"; return 1 ;;
        esac
    done
    return 0
}
