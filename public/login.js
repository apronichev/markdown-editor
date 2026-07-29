/* Login page: surface OAuth errors, warn when the deployment lacks credentials,
 * and carry a post-login redirect through the sign-in link. */

'use strict';

const params = new URLSearchParams(location.search);

const error = params.get('error');
if (error) {
  const box = document.getElementById('error-box');
  box.textContent = error;
  box.hidden = false;
}

// Keep the deep link the user originally asked for, but only same-site paths.
const redirect = params.get('redirect');
if (redirect && redirect.startsWith('/') && !redirect.startsWith('//')) {
  const link = document.getElementById('sign-in');
  link.href = `/api/auth/login?redirect=${encodeURIComponent(redirect)}`;
}

(async () => {
  try {
    const response = await fetch('/api/config', { credentials: 'same-origin' });
    if (!response.ok) return;
    const config = await response.json();
    if (!config.configured) {
      document.getElementById('setup-warning').hidden = false;
      const link = document.getElementById('sign-in');
      link.setAttribute('aria-disabled', 'true');
      link.addEventListener('click', (event) => event.preventDefault());
    }
  } catch {
    /* Offline or the function is cold — the sign-in link still works. */
  }
})();

// If a valid session already exists, skip the login page.
(async () => {
  try {
    const response = await fetch('/api/me', { credentials: 'same-origin' });
    if (!response.ok) return;
    const me = await response.json();
    if (me.authenticated && !error) {
      location.replace(redirect && redirect.startsWith('/') && !redirect.startsWith('//') ? redirect : '/');
    }
  } catch {
    /* Ignore: showing the login page is the safe fallback. */
  }
})();
