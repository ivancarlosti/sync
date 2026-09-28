// reCAPTCHA v3 loader.
//
// The site key comes from the server (`captcha.site_key` of GET
// /api/auth/session`), so an instance without `RECAPTCHA_SITE_KEY` never loads
// the widget and the login form stays a plain two-field form. The script is
// injected only when the operator actually signs in — the CSP allows
// google.com/gstatic.com, and nothing is fetched on a page that does not need it.
const SCRIPT_ID = 'sync-recaptcha';

interface Grecaptcha {
  ready(callback: () => void): void;
  execute(siteKey: string, options: { action: string }): Promise<string>;
}

declare global {
  interface Window {
    grecaptcha?: Grecaptcha;
  }
}

/** loadRecaptcha injects the API script once and resolves when it is ready. */
function loadRecaptcha(): Promise<Grecaptcha> {
  if (window.grecaptcha) {
    return new Promise((resolve) => {
      window.grecaptcha?.ready(() => resolve(window.grecaptcha as Grecaptcha));
    });
  }
  return new Promise((resolve, reject) => {
    const existing = document.getElementById(SCRIPT_ID) as HTMLScriptElement | null;
    const script = existing ?? document.createElement('script');
    script.addEventListener('load', () => {
      if (!window.grecaptcha) {
        reject(new Error('reCAPTCHA did not expose grecaptcha'));
        return;
      }
      window.grecaptcha.ready(() => resolve(window.grecaptcha as Grecaptcha));
    });
    script.addEventListener('error', () => reject(new Error('reCAPTCHA failed to load')));
    if (!existing) {
      script.id = SCRIPT_ID;
      // `render` is unused for v3, but requesting it here keeps the URL explicit
      // about the version the CSP was written for.
      script.src = 'https://www.google.com/recaptcha/api.js?render=explicit';
      script.async = true;
      script.defer = true;
      document.head.appendChild(script);
    }
  });
}

/**
 * captchaToken asks for a v3 token for one action. The caller uses it as the
 * `captcha_token` of the login payload; an empty string means "send the request
 * without a token" and lets the server decide.
 */
export async function captchaToken(siteKey: string, action = 'login'): Promise<string> {
  const grecaptcha = await loadRecaptcha();
  return grecaptcha.execute(siteKey, { action });
}
