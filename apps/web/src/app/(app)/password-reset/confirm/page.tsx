'use client';

import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { Suspense, useState, type FormEvent } from 'react';

import { ApiError, request } from '@/lib/api';
import styles from '../page.module.css';

/**
 * PAROLA SIFIRLAMA — YENİ PAROLA (8.4).
 *
 * BAŞARIDAN SONRA OTURUM AÇILMIYOR: sunucu 204 dönüyor ve kullanıcı giriş
 * ekranına yönlendiriliyor. Sıfırlama az önce kullanıcının TÜM oturumlarını
 * iptal etti; hemen ardından oturum açmak o kararı aynı istekte geri almak
 * olurdu. Yeni parolayla giriş yapmak, parolanın gerçekten değiştiğinin de
 * kanıtı.
 *
 * DOĞRULAMA KURALLARI TEKRARLANMIYOR (2.1'in kararı): parolanın en az 8
 * karakter olması sunucunun kuralı ve 422'nin `fields` dizisi doğrudan
 * girdinin altına yazılıyor.
 */

function PasswordResetConfirmView() {
  const params = useSearchParams();
  const token = params.get('token') ?? '';

  const [password, setPassword] = useState('');
  const [done, setDone] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    setFieldErrors({});

    try {
      await request('/v1/auth/password-reset/confirm', {
        method: 'POST',
        body: { token, password },
      });
      setDone(true);
    } catch (cause) {
      if (cause instanceof ApiError) {
        setFieldErrors(cause.fieldMessages());
        setError(cause.message);
      } else {
        setError('Parola değiştirilemedi');
      }
    } finally {
      setSubmitting(false);
    }
  }

  if (done) {
    return (
      <main className={styles.page}>
        <div className={styles.card}>
          <h1 className={styles.title}>Parolanız değiştirildi</h1>
          <p className={styles.body}>
            Güvenlik için açık olan tüm oturumlar kapatıldı. Yeni parolanızla
            giriş yapabilirsiniz.
          </p>
          <Link className={styles.link} href="/login">
            Giriş yap
          </Link>
        </div>
      </main>
    );
  }

  if (token === '') {
    return (
      <main className={styles.page}>
        <div className={styles.card}>
          <h1 className={styles.title}>Bağlantı eksik</h1>
          <p className={styles.body}>
            E-postanızdaki adresi olduğu gibi kullanın.
          </p>
          <Link className={styles.link} href="/password-reset">
            Yeni bağlantı iste
          </Link>
        </div>
      </main>
    );
  }

  return (
    <main className={styles.page}>
      <form className={styles.card} onSubmit={handleSubmit} noValidate>
        <h1 className={styles.title}>Yeni parola</h1>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}

        <label className={styles.field}>
          <span className={styles.label}>Yeni parola</span>
          <input
            className={styles.input}
            type="password"
            name="password"
            autoComplete="new-password"
            required
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            aria-invalid={fieldErrors.password ? true : undefined}
          />
          {fieldErrors.password && (
            <span className={styles.fieldError}>{fieldErrors.password}</span>
          )}
        </label>

        <button className={styles.submit} type="submit" disabled={submitting}>
          {submitting ? 'Değiştiriliyor…' : 'Parolayı değiştir'}
        </button>

        <Link className={styles.link} href="/password-reset">
          Yeni bağlantı iste
        </Link>
      </form>
    </main>
  );
}

/** `useSearchParams` bir Suspense sınırı istiyor (Next'in kuralı). */
export default function PasswordResetConfirmPage() {
  return (
    <Suspense fallback={<main className={styles.page} />}>
      <PasswordResetConfirmView />
    </Suspense>
  );
}
