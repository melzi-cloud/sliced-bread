#!/usr/bin/env node
// Replaces the default admin password ("umami") with the generated one. Safe to repeat.
const base = process.env.SB_APP_INTERNAL;
const pw = process.env.UMAMI_ADMIN_PASSWORD;

async function login(password) {
  const r = await fetch(base + "/api/auth/login", {
    method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ username: "admin", password }),
  });
  return r.ok ? r.json() : null;
}

if (await login(pw)) {
  console.log(JSON.stringify({ changed: false }));
  process.exit(0);
}
const def = await login("umami");
if (!def) {
  console.error("neither the generated nor the default admin password works");
  process.exit(1);
}
const r = await fetch(`${base}/api/users/${def.user.id}`, {
  method: "POST",
  headers: { "content-type": "application/json", authorization: "Bearer " + def.token },
  body: JSON.stringify({ password: pw }),
});
if (!r.ok) {
  console.error(`setting the admin password failed: HTTP ${r.status}`);
  process.exit(1);
}
console.log(JSON.stringify({ changed: true }));
