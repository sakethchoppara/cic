# Prints SAFE if /work fills up at its tmpfs size (16 MB for python) instead of growing.
import sys

written = 0
try:
    with open("/work/fill", "wb") as f:
        while True:
            f.write(b"\0" * (1 << 20))
            f.flush()
            written += 1
except OSError:
    pass
print("SAFE" if written <= 16 else f"UNSAFE: wrote {written} MB")
sys.exit(0 if written <= 16 else 1)
