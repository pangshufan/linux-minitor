'use strict';
const loginEl = id => document.getElementById(id);
loginEl('login-form').addEventListener('submit', async event => {
  event.preventDefault();
  loginEl('login-submit').disabled = true;
  loginEl('login-error').textContent = '';
  try {
    const response = await fetch('/api/login', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body:JSON.stringify({username:loginEl('username').value, password:loginEl('password').value}),
    });
    const data = await response.json();
    if (!response.ok) throw Error(data.error || '登录失败');
    loginEl('password').value = '';
    const next = new URLSearchParams(location.search).get('next') || '/';
    // Restrict navigation to a path on this origin, including encoded path variants.
    const target = new URL(next, location.origin);
    location.assign(target.origin === location.origin ? target.href : '/');
  } catch (error) {
    loginEl('login-error').textContent = error.message;
    loginEl('login-submit').disabled = false;
  }
});
