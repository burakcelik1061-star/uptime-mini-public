'use client';

import { useRouter } from 'next/navigation';
import { useCallback, useEffect, useState } from 'react';

import { AppHeader } from '@/components/AppHeader';
import { MaintenanceForm } from '@/components/MaintenanceForm';
import { ApiError } from '@/lib/api';
import type {
  CreateMaintenanceWindowInput,
  MaintenanceWindow,
  Monitor,
  Organization,
} from '@/lib/api-types';
import { formatDateTime, maintenancePhase } from '@/lib/format';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * Planlı bakım pencereleri (4.3'ün uçları, 4.4'ün ekranı).
 *
 * Pencerenin ne işe yaradığı ekranda YAZILI: bakım sırasında otomatik incident
 * açılmıyor ve o aralıktaki kontroller uptime'ın paydasından düşülüyor. Bunu
 * söylemeyen bir ekran, kullanıcının "neden kesinti kaydı yok?" sorusunu
 * cevapsız bırakırdı.
 *
 * "Aktif / yaklaşan / geçmiş" ayrımı İSTEMCİDE türetiliyor (4.3'ün kararı):
 * sunucu etiket göndermiyor, çünkü onun "şu an"ı ile istemcininki arasındaki
 * fark aynı pencereyi iki farklı kutuya düşürebilirdi.
 */

const PHASE_LABELS = {
  active: 'Şu anda bakımda',
  upcoming: 'Yaklaşan',
  past: 'Geçmiş',
} as const;

export default function MaintenancePage() {
  const router = useRouter();
  const { status, authFetch } = useSession();

  const [org, setOrg] = useState<Organization | null>(null);
  const [windows, setWindows] = useState<MaintenanceWindow[]>([]);
  const [monitors, setMonitors] = useState<Monitor[]>([]);

  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    if (status === 'anonymous') router.replace('/login');
  }, [status, router]);

  const loadWindows = useCallback(
    async (orgID: string) => {
      const data = await authFetch<{
        maintenanceWindows: MaintenanceWindow[];
      }>(`/v1/orgs/${orgID}/maintenance-windows`);

      setWindows(data.maintenanceWindows);
    },
    [authFetch],
  );

  useEffect(() => {
    if (status !== 'authenticated') return;

    let cancelled = false;

    async function load() {
      setLoading(true);
      setError(null);

      try {
        const { organizations } = await authFetch<{
          organizations: Organization[];
        }>('/v1/orgs');
        if (cancelled) return;

        const first = organizations[0] ?? null;
        setOrg(first);

        if (!first) {
          setWindows([]);
          setMonitors([]);

          return;
        }

        const [, monitorData] = await Promise.all([
          loadWindows(first.id),
          authFetch<{ monitors: Monitor[] }>(`/v1/orgs/${first.id}/monitors`),
        ]);

        if (!cancelled) setMonitors(monitorData.monitors);
      } catch (cause) {
        if (!cancelled) {
          setError(
            cause instanceof ApiError ? cause.message : 'Veriler yüklenemedi',
          );
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    }

    void load();

    return () => {
      cancelled = true;
    };
  }, [status, authFetch, loadWindows]);

  const createWindow = useCallback(
    async (input: CreateMaintenanceWindowInput) => {
      if (!org) return;

      await authFetch(`/v1/orgs/${org.id}/maintenance-windows`, {
        method: 'POST',
        body: input,
      });

      await loadWindows(org.id);
    },
    [org, authFetch, loadWindows],
  );

  const removeWindow = useCallback(
    async (windowID: string) => {
      if (!org) return;

      setBusy(windowID);
      setError(null);

      try {
        await authFetch(`/v1/orgs/${org.id}/maintenance-windows/${windowID}`, {
          method: 'DELETE',
        });
        await loadWindows(org.id);
      } catch (cause) {
        setError(cause instanceof ApiError ? cause.message : 'Silinemedi');
      } finally {
        setBusy(null);
      }
    },
    [org, authFetch, loadWindows],
  );

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  const canManage = org !== null && org.role !== 'member';

  return (
    <div className={styles.shell}>
      <AppHeader org={org} />

      <main className={styles.page}>
        <h1 className={styles.title}>Planlı bakım</h1>

        <p className={styles.lead}>
          Bakım penceresi içinde kapsanan servisler için{' '}
          <strong>otomatik incident açılmaz</strong>; o aralıktaki kontroller
          uptime hesabının paydasından düşülür ve “bakım” olarak ayrıca sayılır.
        </p>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}

        {canManage ? (
          <MaintenanceForm monitors={monitors} onSubmit={createWindow} />
        ) : (
          <p className={styles.muted}>
            Bu organizasyondaki rolünüz salt-okunur; bakım penceresi tanımlamak
            admin ve üzerindedir.
          </p>
        )}

        {loading ? (
          <p className={styles.muted}>Yükleniyor…</p>
        ) : windows.length === 0 ? (
          <p className={styles.empty}>Tanımlı bakım penceresi yok.</p>
        ) : (
          <ul className={styles.list}>
            {windows.map((window) => {
              const phase = maintenancePhase(window);

              return (
                <li key={window.id} className={styles.item}>
                  <div className={styles.itemHead}>
                    <h2 className={styles.itemTitle}>{window.title}</h2>
                    {/* Faz yalnızca renkle değil METİNLE anlatılıyor. */}
                    <span className={`${styles.phase} ${styles[phase]}`}>
                      {PHASE_LABELS[phase]}
                    </span>
                  </div>

                  <p className={`${styles.range} tabularNumbers`}>
                    {formatDateTime(window.startsAt)} →{' '}
                    {formatDateTime(window.endsAt)}
                  </p>

                  {window.description && (
                    <p className={styles.description}>{window.description}</p>
                  )}

                  <p className={styles.services}>
                    {window.monitors.length === 0
                      ? 'Kapsanan servis kaldırılmış'
                      : window.monitors
                          .map((monitor) => monitor.name)
                          .join(', ')}
                  </p>

                  {canManage && phase === 'upcoming' && (
                    <button
                      className={styles.remove}
                      type="button"
                      onClick={() => void removeWindow(window.id)}
                      disabled={busy !== null}
                    >
                      {busy === window.id ? 'Siliniyor…' : 'İptal et'}
                    </button>
                  )}

                  {canManage && phase !== 'upcoming' && (
                    <p className={styles.hint}>
                      Başlamış bir pencere silinemez: o aralık uptime geçmişinde
                      “bakım” olarak sayıldı.
                    </p>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </main>
    </div>
  );
}
