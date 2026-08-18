'use client';

import Link from 'next/link';
import { useRouter, useSearchParams } from 'next/navigation';
import { Suspense, useEffect, useState, type FormEvent } from 'react';

import { ApiError } from '@/lib/api';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * Giriş ve KAYIT ekranı (Faz 2.1 → 8.1).
 *
 * 2.1 kayıt ekranını bilinçli olarak ertelemişti ve bu ekran kullanıcıya
 * `POST /v1/auth/register` yazıyordu — yani "yabancı biri kaydolup
 * kullanabilir" hâli yoktu. 8.1 o ertelemeyi kapatıyor.
 *
 * TEK EKRAN, İKİ MOD (ayrı bir `/register` rotası değil): iki sayfa, aynı
 * kartın, aynı alan bileşenlerinin ve aynı hata gösterme kodunun ikinci bir
 * kopyası demekti — 2.4'ün "düzenleme için ikinci bir MonitorForm yazma"
 * kararının aynısı. Ayrıca kullanıcının en sık yaptığı şey mod değiştirmek
 * ("hesabım var mıydı?") ve o hareket burada bir sayfa yüklemesi değil.
 *
 * SUNUCU UCU DEĞİŞMEDİ: kayıt ucu 1.1'den beri var, giriş ucuyla aynı gövdeyi
 * döndürüyor ve refresh cookie'sini de aynı yerden yazıyor.
 *
 * DOĞRULAMA KURALLARI BURADA TEKRARLANMIYOR (2.1'in kararı): formda `required`
 * dışında sınır yok — parolanın en az 8 karakter olması, e-postanın biçimi ve
 * adın uzunluğu sunucunun kuralı ve 422'nin `fields` dizisi doğrudan ilgili
 * girdinin altına yazılıyor. Sınırları iki yere kopyalamak, altı ay sonra
 * sessizce ayrışan ikinci bir kural seti demekti.
 */

type Mode = 'login' | 'register';

const COPY: Record<
  Mode,
  {
    subtitle: string;
    submit: string;
    pending: string;
    switchPrompt: string;
    switchAction: string;
    failure: string;
  }
> = {
  login: {
    subtitle: 'Yönetim paneline giriş yapın.',
    submit: 'Giriş yap',
    pending: 'Giriş yapılıyor…',
    switchPrompt: 'Hesabınız yok mu?',
    switchAction: 'Hesap oluşturun',
    failure: 'Giriş yapılamadı',
  },
  register: {
    subtitle:
      'Yeni bir hesap açın; ilk organizasyonu sonraki adımda kuracaksınız.',
    submit: 'Hesap oluştur',
    pending: 'Hesap oluşturuluyor…',
    switchPrompt: 'Zaten hesabınız var mı?',
    switchAction: 'Giriş yapın',
    failure: 'Hesap oluşturulamadı',
  },
};

/**
 * safeNext, giriş sonrası dönülecek adresi doğrular (8.3).
 *
 * AÇIK YÖNLENDİRME (open redirect) KAPISI. `?next=` değeri adres çubuğundan
 * geliyor, yani saldırganın yazdığı bir metin: doğrulanmadan
 * `router.replace`e verilseydi, `…/login?next=https://kotu.example` bağlantısı
 * kullanıcıyı KENDİ SİTEMİZDEN çıkarıp sahte bir giriş ekranına götürebilirdi
 * — üstelik tam da az önce parolasını yazdığı için ikna edici olurdu.
 *
 * Kural: yalnızca TEK EĞİK ÇİZGİYLE başlayan yollar. `//kotu.example`
 * elenmeli çünkü tarayıcı onu protokol-göreli MUTLAK adres sayıyor;
 * `/\kotu.example` de elenmeli çünkü bazı tarayıcılar ters eğik çizgiyi
 * eğik çizgi gibi okuyor.
 */
function safeNext(raw: string | null): string {
  if (!raw) return '/';
  if (!raw.startsWith('/')) return '/';
  if (raw.startsWith('//') || raw.startsWith('/\\')) return '/';

  return raw;
}

function LoginView() {
  const router = useRouter();
  const params = useSearchParams();
  const { status, login, register } = useSession();

  const next = safeNext(params.get('next'));

  const [mode, setMode] = useState<Mode>('login');
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [name, setName] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);

  // Oturum zaten açıksa (cookie'den kurtarıldıysa) burada durmanın anlamı yok.
  useEffect(() => {
    if (status === 'authenticated') router.replace(next);
  }, [status, next, router]);

  const copy = COPY[mode];

  /**
   * Mod değişince HATALAR TEMİZLENİYOR ama girilen e-posta/parola KALIYOR:
   * "giriş yapamadım, demek ki hesabım yok" diyen kullanıcı aynı bilgileri
   * ikinci kez yazmak zorunda kalmasın. Eski moda ait bir alan hatası
   * ("parola en az 8 karakter") yeni modda ekranda asılı kalırsa, kullanıcı
   * onu yeni formun hatası sanardı.
   */
  function switchMode() {
    setMode((current) => (current === 'login' ? 'register' : 'login'));
    setError(null);
    setFieldErrors({});
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    setFieldErrors({});

    try {
      if (mode === 'register') {
        await register(email, password, name);
      } else {
        await login(email, password);
      }
      router.replace(next);
    } catch (cause) {
      if (cause instanceof ApiError) {
        // GİRİŞTE ALAN HATASI GELMİYOR ve bu bilinçli (1.1): sunucu
        // "e-posta yok" ile "parola yanlış"ı ayırmıyor, alan bazlı bir
        // ayrıntı da vermiyor. Kayıtta ise 422'nin `fields` dizisi dolu
        // geliyor; aynı kod ikisini de taşıyor çünkü fark sunucunun.
        setFieldErrors(cause.fieldMessages());
        setError(cause.message);
      } else {
        setError(copy.failure);
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <main className={styles.page}>
      <form className={styles.card} onSubmit={handleSubmit} noValidate>
        <header className={styles.header}>
          <h1 className={styles.title}>Uptime Mini</h1>
          <p className={styles.subtitle}>{copy.subtitle}</p>
        </header>

        {error && (
          // role="alert": hata ekran okuyucuya da bildirilsin.
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}

        {mode === 'register' && (
          <label className={styles.field}>
            <span className={styles.label}>Ad soyad</span>
            <input
              className={styles.input}
              type="text"
              name="name"
              autoComplete="name"
              required
              value={name}
              onChange={(event) => setName(event.target.value)}
              aria-invalid={fieldErrors.name ? true : undefined}
            />
            {fieldErrors.name && (
              <span className={styles.fieldError}>{fieldErrors.name}</span>
            )}
          </label>
        )}

        <label className={styles.field}>
          <span className={styles.label}>E-posta</span>
          <input
            className={styles.input}
            type="email"
            name="email"
            autoComplete="email"
            required
            value={email}
            onChange={(event) => setEmail(event.target.value)}
            aria-invalid={fieldErrors.email ? true : undefined}
          />
          {fieldErrors.email && (
            <span className={styles.fieldError}>{fieldErrors.email}</span>
          )}
        </label>

        <label className={styles.field}>
          <span className={styles.label}>Parola</span>
          <input
            className={styles.input}
            type="password"
            name="password"
            // Tarayıcının parola yöneticisi "bu yeni bir parola mı, kayıtlı
            // olan mı" ayrımını bu ipucundan okuyor; yanlışı, kayıt
            // formunda eski parolanın otomatik doldurulması demek.
            autoComplete={
              mode === 'register' ? 'new-password' : 'current-password'
            }
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
          {submitting ? copy.pending : copy.submit}
        </button>

        <p className={styles.hint}>
          {copy.switchPrompt}{' '}
          <button
            className={styles.switch}
            type="button"
            onClick={switchMode}
            disabled={submitting}
          >
            {copy.switchAction}
          </button>
        </p>

        {/* Yalnızca GİRİŞ modunda: yeni hesap açan birinin unutacak bir
            parolası yok ve bağlantı orada gürültü olurdu. */}
        {mode === 'login' && (
          <p className={styles.hint}>
            <Link className={styles.switch} href="/password-reset">
              Parolamı unuttum
            </Link>
          </p>
        )}
      </form>
    </main>
  );
}

/**
 * `useSearchParams` bir Suspense sınırı istiyor (Next'in kuralı): sorgu
 * parametreleri yalnızca istemcide biliniyor.
 */
export default function LoginPage() {
  return (
    <Suspense fallback={<main className={styles.page} />}>
      <LoginView />
    </Suspense>
  );
}
