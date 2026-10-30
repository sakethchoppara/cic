/* Prints SAFE if every privileged operation is refused. */
#define _GNU_SOURCE
#include <sched.h>
#include <stdio.h>
#include <sys/mount.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <netinet/in.h>
#include <unistd.h>

static int fails;
static void must_fail(const char *what, int rc) {
    if (rc != -1) { printf("UNSAFE: %s succeeded\n", what); fails++; }
}

int main(void) {
    must_fail("setuid(0)", setuid(0));
    must_fail("setgid(0)", setgid(0));
    must_fail("mount tmpfs", mount("none", "/tmp", "tmpfs", 0, NULL));
    must_fail("chroot", chroot("/tmp"));
    must_fail("unshare user ns", unshare(CLONE_NEWUSER));
    must_fail("unshare mount ns", unshare(CLONE_NEWNS));
    must_fail("raw socket", socket(AF_INET, SOCK_RAW, IPPROTO_ICMP));
    must_fail("mknod char device", mknod("/tmp/mem", S_IFCHR | 0600, makedev(1, 1)));
    must_fail("sethostname", sethostname("pwned", 5));
    if (!fails) puts("SAFE");
    return fails ? 1 : 0;
}
