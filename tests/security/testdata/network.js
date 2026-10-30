// Prints SAFE if no network is reachable: only loopback, no DNS, no TCP, no UDP.
const fs = require("fs");
const net = require("net");
const dgram = require("dgram");
const dns = require("dns");

const fails = [];
const ifaces = fs.readdirSync("/sys/class/net");
if (ifaces.join() !== "lo") fails.push("interfaces: " + ifaces.join());

function tcp(host, port) {
  return new Promise((resolve) => {
    const s = net.connect({ host, port, timeout: 1000 });
    s.on("connect", () => { fails.push(`tcp ${host}:${port} connected`); s.destroy(); resolve(); });
    s.on("timeout", () => { fails.push(`tcp ${host}:${port} timed out instead of failing`); s.destroy(); resolve(); });
    s.on("error", () => resolve());
  });
}

(async () => {
  // Public internet, and the default Docker bridge gateway (the host).
  await tcp("1.1.1.1", 53);
  await tcp("8.8.8.8", 443);
  await tcp("172.17.0.1", 2375);
  await new Promise((resolve) => dns.lookup("example.com", (err) => { if (!err) fails.push("dns resolved"); resolve(); }));
  await new Promise((resolve) => {
    const u = dgram.createSocket("udp4");
    u.send("x", 53, "1.1.1.1", (err) => { if (!err) fails.push("udp sent"); u.close(); resolve(); });
  });
  console.log(fails.length ? "UNSAFE: " + fails.join(", ") : "SAFE");
  process.exit(fails.length ? 1 : 0);
})();
