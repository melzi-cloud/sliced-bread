#!/usr/bin/env node
console.log(JSON.stringify({ url: process.env.SB_APP_URL, username: "admin", password: process.env.UMAMI_ADMIN_PASSWORD }));
