'use client';

import { useRouter } from 'next/navigation';
import { useCallback, useEffect, useState, type FormEvent } from 'react';

import { AppHeader } from '@/components/AppHeader';
import { ApiError } from '@/lib/api';
import type {
  CreateNotificationChannelInput,
  NotificationChannel,
  Organization,
} from '@/lib/api-types';
import { useSession } from '@/lib/session';
import controls from '@/components/FormControls.module.css';
import styles from './page.module.css';

/**
 * Bildirim kanalları (6.3'ün uçları ve ekranı).
 *
 * EKRAN "BU BİR MOCK" DİYOR ve bu bilinçli: gönderim bugün yalnızca sunucu
 * loguna yazılıyor (docs/0.0-constraints.md §2 — ücretli/hesap gerektiren dış
 * servis yok). Bunu söylemeyen bir ekran, e-postasının gelmediğini gören
 * kullanıcıyı hata aramaya gönderirdi.
 *
 * GÖNDERİM ÖZETİ (gönderildi / bekliyor / başarısız) listede duruyor: "kanal
 * çalışıyor mu" sorusunun cevabı, kullanıcının posta kutusuna bakmasına gerek
 * kalmadan burada.
 */

const KIND_LABELS = {
  email: 'E-posta',
  webhook: 'Webhook',
} as const;

const KIND_HINTS = {
  email: 'Örnek: nobetci@sirket.com',
  webhook: 'https:// ile başlamalı — örnek: https://ornek.com/hooks/uptime',
} as const;

export default function NotificationsPage() {
  const router = useRouter();
  const { status, authFetch } = useSession();

  const [org, setOrg] = useState<Organization | null>(null);
  const [channels, setChannels] = useState<NotificationChannel[]>([]);

  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const [kind, setKind] = useState<'email' | 'webhook'>('email');
  const [name, setName] = useState('');
  const [target, setTarget] = useState('');
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (status === 'anonymous') router.replace('/login');
  }, [status, router]);

  const loadChannels = useCallback(
    async (orgID: string) => {
      const data = await authFetch<{ channels: NotificationChannel[] }>(
        `/v1/orgs/${orgID}/notification-channels`,
      );
      setChannels(data.channels);
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
          setChannels([]);
          return;
        }

        await loadChannels(first.id);
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
  }, [status, authFetch, loadChannels]);

  const submit = useCallback(
    async (event: FormEvent) => {
      event.preventDefault();
      if (!org) return;

      setSubmitting(true);
      setError(null);
      setNotice(null);
      setFieldErrors({});

      const input: CreateNotificationChannelInput = { kind, name, target };

      try {
        await authFetch(`/v1/orgs/${org.id}/notification-channels`, {
          method: 'POST',
          body: input,
        });

        setName('');
        setTarget('');
        await loadChannels(org.id);
      } catch (cause) {
        if (cause instanceof ApiError) {
          // Alan bazlı hatalar sunucudan geliyor: kural servisde yazılı ve
          // arayüz onu tekrar etmiyor (2.1'den beri aynı desen).
          setFieldErrors(cause.fieldMessages());
          setError(cause.message);
        } else {
          setError('Kanal eklenemedi');
        }
      } finally {
        setSubmitting(false);
      }
    },
    [org, kind, name, target, authFetch, loadChannels],
  );

  const sendTest = useCallback(
    async (channelID: string) => {
      if (!org) return;

      setBusy(channelID);
      setError(null);
      setNotice(null);

      try {
        await authFetch(
          `/v1/orgs/${org.id}/notification-channels/${channelID}/test`,
          { method: 'POST' },
        );
        setNotice(
          'Test bildirimi gönderildi — bu kurulumda sunucu loguna yazıldı.',
        );
      } catch (cause) {
        setError(
          cause instanceof ApiError ? cause.message : 'Test gönderilemedi',
        );
      } finally {
        setBusy(null);
      }
    },
    [org, authFetch],
  );

  const removeChannel = useCallback(
    async (channelID: string) => {
      if (!org) return;

      setBusy(channelID);
      setError(null);
      setNotice(null);

      try {
        await authFetch(
          `/v1/orgs/${org.id}/notification-channels/${channelID}`,
          { method: 'DELETE' },
        );
        await loadChannels(org.id);
      } catch (cause) {
        setError(cause instanceof ApiError ? cause.message : 'Silinemedi');
      } finally {
        setBusy(null);
      }
    },
    [org, authFetch, loadChannels],
  );

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  const canManage = org !== null && org.role !== 'member';

  return (
    <div className={styles.shell}>
      <AppHeader org={org} />

      <main className={styles.page}>
        <h1 className={styles.title}>Bildirim kanalları</h1>

        <p className={styles.lead}>
          Bir kesinti <strong>açıldığında ve kapandığında</strong> buradaki
          kanallara bildirim gider. Ara timeline notları bildirim üretmez.
        </p>

        <p className={styles.mock} role="note">
          Bu kurulumda gerçek e-posta/webhook gönderimi <strong>yok</strong>:
          bildirimler sunucu loguna yazılıyor. Gerçek bir sağlayıcı eklemek tek
          bir yapıcı fonksiyonun değişmesi.
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

        {canManage ? (
          <form className={controls.form} onSubmit={(e) => void submit(e)}>
            <div className={controls.row}>
              <label className={controls.field}>
                <span className={controls.label}>Tür</span>
                <select
                  className={controls.input}
                  value={kind}
                  onChange={(e) =>
                    setKind(e.target.value as 'email' | 'webhook')
                  }
                >
                  <option value="email">E-posta</option>
                  <option value="webhook">Webhook</option>
                </select>
              </label>

              <label className={controls.field}>
                <span className={controls.label}>Ad</span>
                <input
                  className={controls.input}
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="Nöbetçi ekip"
                  aria-invalid={fieldErrors.name ? true : undefined}
                />
                {fieldErrors.name && (
                  <span className={controls.fieldError}>
                    {fieldErrors.name}
                  </span>
                )}
              </label>
            </div>

            <label className={controls.field}>
              <span className={controls.label}>Hedef</span>
              <input
                className={controls.input}
                value={target}
                onChange={(e) => setTarget(e.target.value)}
                placeholder={KIND_HINTS[kind]}
                aria-invalid={fieldErrors.target ? true : undefined}
              />
              <span className={controls.hint}>{KIND_HINTS[kind]}</span>
              {fieldErrors.target && (
                <span className={controls.fieldError}>
                  {fieldErrors.target}
                </span>
              )}
            </label>

            <button
              className={controls.submit}
              type="submit"
              disabled={submitting}
            >
              {submitting ? 'Ekleniyor…' : 'Kanal ekle'}
            </button>
          </form>
        ) : (
          <p className={styles.muted}>
            Bu organizasyondaki rolünüz salt-okunur; kanal eklemek admin ve
            üzerindedir.
          </p>
        )}

        {loading ? (
          <p className={styles.muted}>Yükleniyor…</p>
        ) : channels.length === 0 ? (
          <p className={styles.empty}>Tanımlı bildirim kanalı yok.</p>
        ) : (
          <ul className={styles.list}>
            {channels.map((channel) => (
              <li key={channel.id} className={styles.item}>
                <div className={styles.itemHead}>
                  <h2 className={styles.itemTitle}>{channel.name}</h2>
                  {/* Tür renkle değil METİNLE anlatılıyor. */}
                  <span className={styles.kind}>
                    {KIND_LABELS[channel.kind]}
                  </span>
                </div>

                <p className={styles.target}>{channel.target}</p>

                <p className={`${styles.stats} tabularNumbers`}>
                  {channel.deliveries.sent} gönderildi ·{' '}
                  {channel.deliveries.pending} bekliyor ·{' '}
                  {channel.deliveries.failed} başarısız
                </p>

                {canManage && (
                  <div className={styles.actions}>
                    <button
                      className={styles.test}
                      type="button"
                      onClick={() => void sendTest(channel.id)}
                      disabled={busy !== null}
                    >
                      {busy === channel.id ? 'Gönderiliyor…' : 'Test gönder'}
                    </button>
                    <button
                      className={styles.remove}
                      type="button"
                      onClick={() => void removeChannel(channel.id)}
                      disabled={busy !== null}
                    >
                      Sil
                    </button>
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </main>
    </div>
  );
}
