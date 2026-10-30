# Prints SAFE if the container shows no route to the host. Stdin: the host's hostname.
import os, socket, sys

host_hostname = sys.stdin.read().strip()
fails = []

def check(name, ok):
    if not ok:
        fails.append(name)

check("own pid namespace", open("/proc/1/cmdline", "rb").read().split(b"\0")[0] == b"sleep")
check("own uts namespace", socket.gethostname() != host_hostname)
for sock in ["/var/run/docker.sock", "/run/docker.sock", "/run/containerd/containerd.sock"]:
    check("no " + sock, not os.path.exists(sock))
try:
    open("/etc/shadow").read()
    check("shadow unreadable", False)
except PermissionError:
    pass

status = dict(l.split(":\t", 1) for l in open("/proc/self/status").read().splitlines() if ":\t" in l)
check("non-root", os.getuid() != 0)
check("no effective caps", int(status["CapEff"], 16) == 0)
check("no bounding caps", int(status["CapBnd"], 16) == 0)
check("no_new_privs", status["NoNewPrivs"].strip() == "1")
check("seccomp filter", status["Seccomp"].strip() == "2")

# Only the expected mounts: nothing bind-mounted in from the host.
allowed = ("/proc", "/dev", "/sys", "/tmp", "/work", "/etc/resolv.conf", "/etc/hostname", "/etc/hosts")
for line in open("/proc/mounts"):
    _, target, _, opts, *_ = line.split()
    if target == "/":
        check("read-only rootfs", "ro" in opts.split(","))
    elif target == "/sys/fs/cgroup":
        check("read-only cgroupfs", "ro" in opts.split(","))
    elif not target.startswith(allowed):
        check("unexpected mount " + target, False)

print("SAFE" if not fails else "UNSAFE: " + ", ".join(fails))
sys.exit(1 if fails else 0)
