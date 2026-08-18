'use client';

import { useRouter } from 'next/navigation';
import { useEffect, useState } from 'react';

import { AppHeader } from '@/components/AppHeader';
import { ApiError } from '@/lib/api';
import type { Organization, OrgPlan, PlanState } from '@/lib/api-types';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * PLAN & KOTA EKRANI (8.5).
 *
 * SINIRLAR SUNUCUDAN GELİYOR ve ekran onları TEKRARLAMIYOR: "5 monitor",
 * "300 saniye" gibi sayılar burada sabit değil, `GET …/plan` cevabından
 * çiziliyor (2.1'in "doğrulama kuralları istemcide tekrarlanmaz" kararı).
 *
 * ÖDEME MOCK ve ekran bunu AÇIKÇA yazıyor (6.3'ün bildirim ekranıyla aynı
 * disiplin): tahsilat yapılmıyor. Söylemeyen bir ekran, kart bilgisi bekleyen
 * kullanıcıyı yanıltırdı.
 */

const PLAN_ORDER: OrgPlan[] = ['free', 'pro'];

export default function PlanPage() {
  const router = useRouter();
  const { status, authFetch } = useSession();

  const [org, setOrg] = useState<Organization | null>(null);
  const [plan, setPlan] = useState<PlanState | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState<OrgPlan | null>(null);
  const [reloadToken, setReloadToken] = useState(0);

  useEffect(() => {
    if (status === 'anonymous') router.replace('/login');
  }, [status, router]);

  useEffect(() => {
    if (status !== 'authenticated') return;

    let cancelled = false;

    async function load() {
      setError(null);

      try {
        const { organizations } = await authFetch<{
          organizations: Organization[];
        }>('/v1/orgs');
        if (cancelled) return;

        const first = organizations[0] ?? null;
        setOrg(first);

        if (!first) {
          setPlan(null);
          return;
        }

        const data = await authFetch<PlanState>(`/v1/orgs/${first.id}/plan`);
        if (cancelled) return;
        setPlan(data);
      } catch (cause) {
        if (cancelled) return;
        setError(cause instanceof ApiError ? cause.message : 'Plan okunamadı');
      }
    }

    void load();

    return () => {
      cancelled = true;
    };
  }, [status, authFetch, reloadToken]);

  async function changePlan(target: OrgPlan) {
    if (!org) return;

    setBusy(target);
    setError(null);
    setNotice(null);

    try {
      await authFetch(`/v1/orgs/${org.id}/plan`, {
        method: 'PUT',
        body: { plan: target },
      });
      setNotice('Plan güncellendi.');
      setReloadToken((token) => token + 1);
    } catch (cause) {
      setError(
        cause instanceof ApiError ? cause.message : 'Plan değiştirilemedi',
      );
    } finally {
      setBusy(null);
    }
  }

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  // Plan değiştirmek OWNER'a özel (sunucudaki kural). Arayüz kuralı
  // TEKRARLAMIYOR, yalnızca çıkmaz yola sokmuyor: owner olmayan kullanıcıya
  // düğme gösterilmiyor ama reddi yapan yer sunucu (403).
  const canChange = org?.role === 'owner';

  return (
    <>
      <AppHeader org={org} />

      <main className={styles.page}>
        <h1 className={styles.title}>Plan</h1>

        <p className={styles.mockNotice} role="note">
          <strong>Ödeme mock.</strong> Bu projede ücretli bir sağlayıcı yok:
          plan değiştirmek gerçek bir tahsilat yapmıyor, yalnızca kotaların
          nasıl açılıp kapandığını gösteriyor. Gerçek sağlayıcıya geçiş tek bir
          yapıcı fonksiyonda (
          <code className={styles.code}>billing.NewProvider</code>).
        </p>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}
        {notice && (
          <p className={styles.notice} role="status">
            {notice}
          </p>
        )}

        {!org && (
          <p className={styles.empty}>
            Önce bir organizasyon açın; plan organizasyona ait.
          </p>
        )}

        {plan && (
          <>
            <section className={styles.usage}>
              <h2 className={styles.sectionTitle}>Kullanım</h2>
              <p className={styles.usageLine}>
                <strong>{plan.usage.monitors}</strong> /{' '}
                {plan.limits.maxMonitors} monitor kullanılıyor.
              </p>
              <p className={styles.usageLine}>
                En sık kontrol aralığı:{' '}
                <strong>{plan.limits.minCheckIntervalSeconds} saniye</strong>
              </p>
            </section>

            <section>
              <h2 className={styles.sectionTitle}>Planlar</h2>
              <ul className={styles.list}>
                {PLAN_ORDER.map((value) => {
                  const option = plan.available.find((p) => p.plan === value);
                  if (!option) return null;

                  const current = option.plan === plan.plan;

                  return (
                    <li
                      key={option.plan}
                      className={
                        current
                          ? `${styles.card} ${styles.currentCard}`
                          : styles.card
                      }
                    >
                      <div className={styles.cardHead}>
                        <span className={styles.planName}>{option.label}</span>
                        {/* "Mevcut plan" renkle değil METİNLE (0.2'nin şartı). */}
                        {current && (
                          <span className={styles.badge}>Mevcut plan</span>
                        )}
                      </div>

                      <ul className={styles.features}>
                        <li>{option.limits.maxMonitors} monitor</li>
                        <li>
                          {option.limits.minCheckIntervalSeconds} saniyeye kadar
                          sık kontrol
                        </li>
                      </ul>

                      {!current && canChange && (
                        <button
                          className={styles.action}
                          type="button"
                          onClick={() => void changePlan(option.plan)}
                          disabled={busy !== null}
                        >
                          {busy === option.plan
                            ? 'Uygulanıyor…'
                            : `${option.label} planına geç`}
                        </button>
                      )}
                    </li>
                  );
                })}
              </ul>
            </section>
          </>
        )}
      </main>
    </>
  );
}
