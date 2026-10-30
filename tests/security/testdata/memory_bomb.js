// Buffers live outside the V8 heap, so this hits the cgroup limit, not V8's.
const chunks = [];
for (;;) chunks.push(Buffer.alloc(10 << 20, 1));
