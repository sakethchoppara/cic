# 6. Source delivery over exec stdin

**Decision:** Source code reaches `/work` by running `sh -c 'cat > /work/<file>'` as an exec and streaming the code into its stdin. There are no bind mounts, and the Docker archive (copy) API isn't used.

**Context:** The root filesystem is read-only, and `/work` is a tmpfs created with the container.

**Why not bind mounts:** A bind mount means host files the runner writes for each request, which is a host-side cleanup problem and a path from request data to the host filesystem. It also can't be added to an already-running pooled container.

**Why not the archive API:** The archive API (`docker cp`) was expected not to write into tmpfs mounts and to fail against a read-only root. This came from memory of a moby issue and was never verified in CIC. Exec stdin was chosen because it works regardless, and it reuses the exec and attach code the runner already needs for compile and run.

**Consequences:**
- One extra exec per run (create, attach, inspect). It wasn't measured on its own. All of a warm Python hello world takes about 130 ms end to end, including this step, the run, and the memory-kill check.
- The source file name comes from the config, not the request, so nothing from the request is interpolated into the `sh -c` string. If a language config ever took the file name from user input, this would become a shell-injection bug.
- Requires `sh` and `cat` in every image, which all the official images have.
