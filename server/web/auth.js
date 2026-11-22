'use strict';
function goToLogin() {
  location.replace('/login.html?next=' + encodeURIComponent(location.pathname + location.search + location.hash));
}
(async () => {
  try {
    const response = await fetch('/api/session');
    if (response.status === 401) { goToLogin(); return; }
    if (!response.ok) return;
    const session = await response.json();
    const target = document.querySelector('.sidebar-note');
    if (!target) return;
    const account = document.createElement('div');
    account.className = 'account-panel';
    const user = document.createElement('span');
    user.textContent = session.username;
    const logout = document.createElement('button');
    logout.type = 'button'; logout.textContent = '退出登录';
    logout.addEventListener('click', async () => {
      logout.disabled = true;
      try {
        const response = await fetch('/api/logout', {method:'POST', headers:{'Content-Type':'application/json'},body:'{}'});
        if (response.ok || response.status === 401) location.replace('/login.html');
        else { logout.textContent = '退出失败，重试'; logout.disabled = false; }
      } catch { logout.textContent = '退出失败，重试'; logout.disabled = false; }
    });
    account.append(user,logout);
    target.prepend(account);
    target.classList.add("has-account");
  } catch { /* Existing page refresh reports network failures without losing the view. */ }
})();
