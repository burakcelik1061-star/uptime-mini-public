import styles from './not-found.module.css';

/**
 * Bilinmeyen (veya yayımlanmamış) bir status page için 404 (5.1).
 *
 * ANLAMLI OLMASI ŞART ama BİLGİ VERMEMESİ de şart: "bu sayfa var ama
 * yayımlanmamış" demek, denenen adreslerden hangi organizasyonların kayıtlı
 * olduğunu dışarıya söylemek olurdu. Sunucu da bu iki durumu ayırt etmiyor —
 * metin ikisini birden karşılıyor.
 *
 * 5.4'te bilinmeyen ALT ALAN ADI da bu sayfaya düşecek.
 */
export default function StatusPageNotFound() {
  return (
    <main className={styles.page}>
      <h1 className={styles.title}>Bu adreste bir status page yok</h1>

      <p className={styles.body}>
        Adres yanlış yazılmış olabilir ya da sayfa henüz yayımlanmamış olabilir.
        Bağlantıyı size veren ekibe danışabilirsiniz.
      </p>
    </main>
  );
}
