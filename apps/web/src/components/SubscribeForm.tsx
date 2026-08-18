import { subscribeToStatusPage } from '@/lib/public-api';
import styles from './SubscribeForm.module.css';

/**
 * Status sayfasına e-posta aboneliği (Faz 6.4).
 *
 * ZİYARETÇİ İLK KEZ YAZIYOR: 0.1 §2'nin "ziyaretçi hiçbir şey yazmaz"
 * kuralının bilinçli istisnası ve bu sayfadaki tek form.
 *
 * SUNUCU ACTION'I, istemci bileşeni DEĞİL. Sebebi 5.1'in kararı: public sayfa
 * sunucuda render ediliyor ve ziyaretçinin tarayıcısı api'ye hiç istek
 * atmıyor. Form Next sunucusuna gidiyor, api çağrısını o yapıyor —
 * dolayısıyla CORS bu yola yine girmiyor ve **JavaScript kapalıyken de
 * çalışıyor** (ilerlemeli geliştirme: form, `action` ile gönderiliyor).
 *
 * SONUÇ MESAJI HER DURUMDA AYNI: "adresinize bir onay bağlantısı gönderildi".
 * Sunucu da böyle cevaplıyor — "bu adres zaten kayıtlı" demek, o sayfaya
 * kimin abone olduğunu dışarıya söylemek olurdu (e-posta enumeration).
 */

const MESSAGES = {
  ok: 'Adresinize bir onay bağlantısı gönderildi. Aboneliğiniz onaydan sonra başlar.',
  invalid: 'Geçerli bir e-posta adresi girin.',
  error: 'Şu anda işlenemedi, biraz sonra tekrar deneyin.',
} as const;

export function SubscribeForm({
  slug,
  result,
}: {
  slug: string;
  result?: keyof typeof MESSAGES;
}) {
  async function subscribe(formData: FormData) {
    'use server';

    const { redirect } = await import('next/navigation');
    const email = String(formData.get('email') ?? '');
    const outcome = await subscribeToStatusPage(slug, email);

    // Sonuç ADRES SATIRINDA taşınıyor: sayfa sunucuda render edildiği için
    // istemci state'i yok ve yenilemede formun yeniden gönderilmesi
    // (POST/Redirect/GET) böylece engelleniyor.
    redirect(`/status/${slug}?subscribe=${outcome}`);
  }

  return (
    <section className={styles.box} aria-labelledby="subscribe-heading">
      <h2 className={styles.heading} id="subscribe-heading">
        Kesinti bildirimi al
      </h2>

      <p className={styles.lead}>
        Bir kesinti açıldığında ve kapandığında e-posta gönderilir. Her
        bildirimde abonelikten çıkma bağlantısı bulunur.
      </p>

      <form className={styles.form} action={subscribe}>
        <label className={styles.label} htmlFor="subscribe-email">
          E-posta adresi
        </label>
        <div className={styles.row}>
          <input
            className={styles.input}
            id="subscribe-email"
            name="email"
            type="email"
            required
            placeholder="siz@ornek.com"
            aria-describedby={result ? 'subscribe-result' : undefined}
          />
          <button className={styles.submit} type="submit">
            Abone ol
          </button>
        </div>
      </form>

      {result && (
        // Sonuç renkle DEĞİL metinle anlatılıyor (0.2'nin şartı).
        <p className={styles.result} id="subscribe-result" role="status">
          {MESSAGES[result]}
        </p>
      )}
    </section>
  );
}
