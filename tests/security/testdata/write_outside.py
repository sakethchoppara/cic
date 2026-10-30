# Prints SAFE if only /work and /tmp are writable.
import sys

fails = []
for path in ["/x", "/etc/x", "/usr/x", "/root/x", "/home/x", "/var/x", "/opt/x",
             "/sys/fs/cgroup/x", "/proc/sys/kernel/hostname"]:
    try:
        with open(path, "w") as f:
            f.write("x")
        fails.append("wrote " + path)
    except OSError:
        pass
for path in ["/work/x", "/tmp/x"]:
    try:
        with open(path, "w") as f:
            f.write("x")
    except OSError as e:
        fails.append(f"can't write {path}: {e}")

print("SAFE" if not fails else "UNSAFE: " + ", ".join(fails))
sys.exit(1 if fails else 0)
