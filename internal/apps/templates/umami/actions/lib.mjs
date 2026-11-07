// Shared by the Umami actions: log in as admin and call the API.
export const base = process.env.SB_APP_INTERNAL;

export async function login() {
  const r = await fetch(base + "/api/auth/login", {
    method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ username: "admin", password: process.env.UMAMI_ADMIN_PASSWORD }),
  });
  if (!r.ok) fail(`umami login failed: HTTP ${r.status} (is the app still starting? run sb wait)`);
  return (await r.json()).token;
}

export async function api(token, path, body) {
  const r = await fetch(base + path, {
    method: body ? "POST" : "GET",
    headers: { "content-type": "application/json", authorization: "Bearer " + token },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!r.ok) fail(`${path}: HTTP ${r.status}: ${(await r.text()).slice(0, 200)}`);
  return r.json();
}

// Umami's stat values are numbers or {value: n}, depending on the version.
export const num = (v) => (v && typeof v === "object" ? v.value : v);

export function fail(msg) {
  console.error(msg);
  process.exit(1);
}

export function done(obj) {
  console.log(JSON.stringify(obj));
}
