'use client';

import Link from 'next/link';
import { useRouter, useSearchParams } from 'next/navigation';
import { Suspense, useEffect, useState } from 'react';

import { ApiError } from '@/lib/api';
import type { AcceptedOrganization } from '@/lib/api-types';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * DAVETİ KABUL ETME (8.3).
 *
 * Akış: e-postadaki link → bu sayfa. Kullanıcı oturumsuzsa önce giriş/kayıt
 * ekranına gidiyor ve `?next=` ile BURAYA geri dönüyor — davet edilen kişinin
 * çoğu zaman henüz hesabı yok, yani "önce kaydol" adımı akışın kendisi.
 *
 * KABUL OTOMATİK DEĞİL, DÜĞMEYE BAĞLI: linki açmak tek başına organizasyona
 * katılmak olmamalı. 6.4'ün abonelikten çıkma ucu bilinçli olarak tek
 * tıklamaydı (ayrılmaya engel koymamak için); burada yön TERS — bir
 * organizasyona KATILMAK, link ön-yükleyici bir istemcinin sessizce
 * yapabileceği bir şey olmamalı.
 */

function AcceptInvitationView() {
  const router = useRouter();
  const params = useSearchParams();
  const { status, authFetch } = useSession();

  const token = params.get('token') ?? '';

  const [org, setOrg] = useState<AcceptedOrganization | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  // Oturumsuz kullanıcıyı giriş ekranına gönderiyoruz ve DÖNÜŞ ADRESİNİ
  // taşıyoruz; aksi hâlde kullanıcı giriş yaptıktan sonra panoya düşer ve
  // daveti kabul etmek için e-postasına geri dönmek zorunda kalırdı.
  useEffect(() => {
    if (status !== 'anonymous') return;

    const next = `/invitations/accept?token=${encodeURIComponent(token)}`;
    router.replace(`/login?next=${encodeURIComponent(next)}`);
  }, [status, token, router]);

  async function handleAccept() {
    setSubmitting(true);
    setError(null);

    try {
      const data = await authFetch<{ organization: AcceptedOrganization }>(
        '/v1/invitations/accept',
        { method: 'POST', body: { token } },
      );
      setOrg(data.organization);
    } catch (cause) {
      setError(
        cause instanceof ApiError ? cause.message : 'Davet kabul edilemedi',
      );
    } finally {
      setSubmitting(false);
    }
  }

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  if (org) {
    return (
      <main className={styles.page}>
        <div className={styles.card}>
          <h1 className={styles.title}>{org.name} ekibine katıldınız</h1>
          <p className={styles.body}>
            Artık bu organizasyonun monitörlerini ve kesintilerini
            görebilirsiniz.
          </p>
          <Link className={styles.action} href="/">
            Panoya git
          </Link>
        </div>
      </main>
    );
  }

  return (
    <main className={styles.page}>
      <div className={styles.card}>
        <h1 className={styles.title}>Davetiniz var</h1>

        {token === '' ? (
          <p className={styles.body}>
            Bağlantı eksik görünüyor. E-postanızdaki adresi olduğu gibi
            kullanın.
          </p>
        ) : (
          <>
            <p className={styles.body}>
              Bir organizasyona davet edildiniz. Katılmak için onaylayın.
            </p>

            {error && (
              <p className={styles.error} role="alert">
                {error}
              </p>
            )}

            <button
              className={styles.action}
              type="button"
              onClick={() => void handleAccept()}
              disabled={submitting}
            >
              {submitting ? 'Katılınıyor…' : 'Daveti kabul et'}
            </button>
          </>
        )}
      </div>
    </main>
  );
}

/**
 * `useSearchParams` bir Suspense sınırı istiyor (Next'in kuralı): sorgu
 * parametreleri yalnızca istemcide biliniyor.
 */
export default function AcceptInvitationPage() {
  return (
    <Suspense fallback={<main className={styles.page}>Yükleniyor…</main>}>
      <AcceptInvitationView />
    </Suspense>
  );
}
