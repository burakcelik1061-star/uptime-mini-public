'use client';

import { useRouter } from 'next/navigation';
import { useEffect, useState, type ReactNode } from 'react';

import { AppHeader } from '@/components/AppHeader';
import { ApiError } from '@/lib/api';
import type { Organization, OutboxMessage } from '@/lib/api-types';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * GELİŞTİRİCİ POSTA KUTUSU (8.2) — ürün ekranı DEĞİL, geliştirme aracı.
 *
 * NEDEN VAR: e-posta bu projede bilinçli olarak MOCK (docs/0.0-constraints.md
 * §2) ve mock adapter gönderimi yalnızca loga yazıyordu. Yani 6.4'ün çift onay
 * akışını denemenin tek yolu `docker compose logs` okumaktı — akışın
 * ÇALIŞTIĞINI göstermek için terminal açmak gerekiyordu. Bu ekran o linkleri
 * tıklanabilir hâle getiriyor.
 *
 * KAPI SUNUCUDA: uç yalnızca APP_ENV açıkça development|test iken router'a
 * kaydediliyor. Arayüz kapıyı TEKRARLAMIYOR (4.4'ün kararı: kural sunucuda,
 * arayüz yalnızca çıkmaz yola sokmuyor) — 404 gelirse "kapalı" diyor.
 */

/** URL yakalayıcı: yalnızca http(s), yalnızca boşluğa kadar. */
const URL_PATTERN = /(https?:\/\/[^\s]+)/g;

/**
 * Parçanın link olup olmadığını söyleyen AYRI ve GLOBAL OLMAYAN kalıp.
 *
 * `URL_PATTERN.test(...)` yazılamaz: `/g` bayraklı bir düzenli ifadenin
 * `test`i `lastIndex`i ilerletiyor, yani ardışık çağrılar dönüşümlü olarak
 * yanlış cevap verir (link olan parça "değil" sayılır). Kalıbı iki kez yazmak
 * yerine ikinci bir bayraksız kopya tutmak, o sessiz hatayı imkânsız kılıyor.
 */
const URL_TEST = /^https?:\/\//;

/**
 * linkify, düz metindeki adresleri tıklanabilir yapar.
 *
 * `dangerouslySetInnerHTML` KULLANILMIYOR: gövde sunucunun ürettiği bir metin
 * ama içinde kullanıcı verisi (organizasyon adı, monitor adı) taşıyor. HTML
 * olarak basmak, o adlardan birine yazılmış bir `<script>`i çalıştırmak
 * demekti. Burada metin React DÜĞÜMLERİNE bölünüyor, yani her parça kaçışlı.
 *
 * Şema http(s) ile sınırlı: `javascript:` ve `data:` böylece hiç eşleşmiyor
 * (5.3'ün logo ve 6.3'ün webhook kararlarının aynısı).
 */
function linkify(text: string): ReactNode[] {
  // Anahtar olarak dizin kullanılıyor: parçaların kimliği yok, dizi yalnızca
  // bu metin için üretiliyor ve sırası hiç değişmiyor.
  return text.split(URL_PATTERN).map((part, index) =>
    URL_TEST.test(part) ? (
      <a
        key={index}
        className={styles.link}
        href={part}
        // Onay/iptal linkleri tek kullanımlık: aynı sekmede açmak posta
        // kutusunu kaybettiriyor.
        target="_blank"
        rel="noreferrer"
      >
        {part}
      </a>
    ) : (
      <span key={index}>{part}</span>
    ),
  );
}

function formatTime(value: string): string {
  return new Date(value).toLocaleString('tr-TR');
}

export default function MailboxPage() {
  const router = useRouter();
  const { status, authFetch } = useSession();

  const [org, setOrg] = useState<Organization | null>(null);
  const [messages, setMessages] = useState<OutboxMessage[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // disabled, sunucunun kapıyı kapalı tuttuğunu söylüyor (404).
  const [disabled, setDisabled] = useState(false);
  // reloadToken, "Yenile" düğmesinin tetikleyicisi. Yükleyici efektin İÇİNDE
  // duruyor (panonun diğer ekranlarıyla aynı desen); dışarıdan tetiklemenin
  // yolu da bu yüzden bir bağımlılık değeri.
  const [reloadToken, setReloadToken] = useState(0);

  useEffect(() => {
    if (status === 'anonymous') router.replace('/login');
  }, [status, router]);

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
        setOrg(organizations[0] ?? null);

        const data = await authFetch<{ messages: OutboxMessage[] }>(
          '/v1/dev/outbox',
        );
        if (cancelled) return;
        setMessages(data.messages);
        setDisabled(false);
      } catch (cause) {
        if (cancelled) return;

        // 404 = uç router'a hiç kaydedilmemiş, yani posta kutusu KAPALI.
        // Bu bir hata değil, bir yapılandırma hâli — ayrı gösteriliyor.
        if (cause instanceof ApiError && cause.status === 404) {
          setDisabled(true);
          setMessages([]);
        } else {
          setError(
            cause instanceof ApiError
              ? cause.message
              : 'Posta kutusu okunamadı',
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
  }, [status, authFetch, reloadToken]);

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  return (
    <>
      <AppHeader org={org} />

      <main className={styles.page}>
        <header className={styles.header}>
          <h1 className={styles.title}>Posta kutusu</h1>
          <button
            className={styles.refresh}
            type="button"
            onClick={() => setReloadToken((token) => token + 1)}
            disabled={loading}
          >
            {loading ? 'Yükleniyor…' : 'Yenile'}
          </button>
        </header>

        {/* MOCK OLDUĞU AÇIKÇA YAZIYOR (6.3'ün ekranıyla aynı disiplin):
            gerçek e-posta iddiası yok. Bunu söylemeyen bir ekran,
            e-postasının gelmediğini gören kullanıcıyı hata aramaya
            gönderirdi. */}
        <p className={styles.mockNotice} role="note">
          <strong>Bu gerçek bir posta kutusu değil.</strong> E-posta bu projede
          mock: gönderimler dışarıya çıkmıyor, sunucu loguna yazılıyor ve burada
          saklanıyor. Gerçek sağlayıcıya geçiş tek bir yapıcı fonksiyonda (
          <code className={styles.code}>notifications.NewRegistry</code>).
        </p>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}

        {disabled && (
          <p className={styles.empty}>
            Posta kutusu kapalı. Yalnızca{' '}
            <code className={styles.code}>APP_ENV</code> açıkça{' '}
            <code className={styles.code}>development</code> veya{' '}
            <code className={styles.code}>test</code> iken açılıyor — bu ekranın
            gösterdiği onay, davet ve parola sıfırlama bağlantıları düz metin.
          </p>
        )}

        {!disabled && !loading && messages.length === 0 && (
          <p className={styles.empty}>
            Henüz gönderilmiş bir e-posta yok. Bir status sayfasına abone olun,
            ekip arkadaşı davet edin veya parola sıfırlama isteyin.
          </p>
        )}

        <ol className={styles.list}>
          {messages.map((message) => (
            <li key={message.id} className={styles.message}>
              <div className={styles.meta}>
                <span className={styles.subject}>{message.subject}</span>
                <time className={styles.time} dateTime={message.createdAt}>
                  {formatTime(message.createdAt)}
                </time>
              </div>
              <div className={styles.recipient}>
                Alıcı:{' '}
                <span className={styles.address}>{message.recipient}</span>
              </div>
              {/* Gövde düz metin: satır sonları korunuyor (CSS
                  `white-space: pre-wrap`) ve linkler tıklanabilir. */}
              <p className={styles.body}>{linkify(message.body)}</p>
            </li>
          ))}
        </ol>
      </main>
    </>
  );
}
