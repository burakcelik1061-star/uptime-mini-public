import { expect, test } from '@playwright/test';

import {
  firstOrgID,
  logIn,
  monitorIDByName,
  newCredentials,
  setFailureThreshold,
} from '../support/api';
import { ControlledTarget } from '../support/target';

/**
 * ANA AKIŞ (7.2): kayıt ol → org aç → monitor ekle → hedefi düşür →
 * kesintinin OTOMATİK açıldığını gör → public status sayfasında görün.
 *
 * Tek bir test: adımlar birbirinin çıktısını kullanıyor ve her adımı ayrı bir
 * teste bölmek, her birinde yığını yeniden kurmak (ya da testler arası gizli
 * bir sıra varsaymak) demekti. Kesintinin oluşması gerçek zaman istiyor —
 * monitor aralığının alt sınırı 30 saniye (2.1).
 *
 * 8.1'DEN BERİ İLK ADIM DA TARAYICIDAN: kayıt ekranı geldi, yani "yabancı
 * biri kaydolup kullanabiliyor" iddiası artık gerçekten ölçülüyor (7.2 bu
 * adımı, arayüzü olmadığı için api'den geçiyordu).
 */
test('kesinti otomatik açılıyor ve status sayfasında görünüyor', async ({
  page,
}) => {
  const target = await ControlledTarget.start();

  try {
    const account = newCredentials();

    // --- Kayıt (arayüzden, 8.1) ----------------------------------------------
    // Giriş formu açılıyor; kayıt AYNI ekranın ikinci modu.
    await page.goto('/login');
    await page
      .getByRole('button', { name: 'Hesap oluşturun', exact: true })
      .click();

    await page.getByLabel('Ad soyad').fill(account.name);
    await page.getByLabel('E-posta').fill(account.email);
    await page.getByLabel('Parola').fill(account.password);
    // `exact` ŞART: Playwright erişilebilir adı varsayılan olarak ALT DİZE
    // eşleştiriyor ve "Hesap oluştur" ile mod değiştiren "Hesap oluşturun"
    // aynı ekranda birlikte bulunabiliyor.
    await page
      .getByRole('button', { name: 'Hesap oluştur', exact: true })
      .click();

    // Kayıt oturumu AÇIK bırakıyor: ayrıca giriş yapmak gerekmiyor
    // (register ucu 1.1'den beri giriş ucuyla aynı gövdeyi döndürüyor).

    // --- Organizasyon (arayüzden) --------------------------------------------
    await expect(
      page.getByRole('heading', { name: 'Önce bir organizasyon açın' }),
    ).toBeVisible();
    await page.getByLabel('Organizasyon adı').fill('E2E Şirketi');
    await page.getByRole('button', { name: 'Organizasyon aç' }).click();

    // --- Plan: PRO'ya yükselt (arayüzden, 8.5) -------------------------------
    //
    // YENİ ORGANİZASYON **FREE** PLANDA ve FREE'nin kontrol aralığı alt sınırı
    // 300 saniye. Bu test kesintinin oluşmasını gerçek zamanda bekliyor, yani
    // 30 saniyelik aralık ŞART (aşağıda `setFailureThreshold` onu ayarlıyor).
    //
    // KURAL ESNETİLMİYOR, ÜRÜN KULLANILIYOR: 30 saniye PRO planın meşru bir
    // hâli ve yükseltme ürünün kendi ekranından, mock ödemeyle yapılıyor —
    // yani bu adım aynı zamanda 8.5'in "duvarı aşmak işe yarıyor" iddiasının
    // uçtan uca kanıtı (7.2'nin `failureThreshold` kararıyla aynı disiplin).
    await page.getByRole('link', { name: 'Plan', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Plan' })).toBeVisible();

    await page.getByRole('button', { name: 'Pro planına geç' }).click();
    await expect(page.getByText('Plan güncellendi.')).toBeVisible();

    // --- Monitor (arayüzden) -------------------------------------------------
    await page.getByRole('link', { name: 'Monitörler' }).click();
    await expect(
      page.getByRole('heading', { name: 'Monitörler' }),
    ).toBeVisible();

    const monitorName = 'Ödeme Servisi';
    await page.getByLabel('Ad', { exact: true }).fill(monitorName);
    await page.getByLabel('URL', { exact: true }).fill(target.url);
    await page.getByRole('button', { name: 'Monitor ekle' }).click();

    const row = page.getByRole('row', { name: new RegExp(monitorName) });
    await expect(row).toBeVisible();

    // Eşiği 1'e çek: 3 eşik × 30 sn aralık, kesintiyi görmek için 90+ saniye
    // beklemek demekti. Formda eşik alanı yok — gerekçe support/api.ts'te.
    // Token api'den alınıyor: arayüz access token'ı yalnızca BELLEKTE
    // tutuyor (2.1'in XSS kararı), yani testin onu sayfadan okuması mümkün
    // değil ve olmamalı.
    const token = await logIn(account);
    const orgID = await firstOrgID(token);
    const monitorID = await monitorIDByName(token, orgID, monitorName);
    await setFailureThreshold(token, orgID, monitorID, 1);

    // İlk kontrol: worker monitor'ü sıraya alıp SAĞLIKLI buluyor.
    await expect(row.getByText('Çalışıyor')).toBeVisible({ timeout: 60_000 });

    // --- Status page: servisi yayımla (arayüzden) ----------------------------
    await page.getByRole('link', { name: 'Status page' }).click();
    await expect(
      page.getByRole('heading', { name: 'Status page', level: 1 }),
    ).toBeVisible();

    await page.getByRole('checkbox', { name: new RegExp(monitorName) }).check();
    await page
      .getByRole('button', { name: 'Servisleri ve grupları kaydet' })
      .click();
    await page.getByRole('button', { name: 'Yayımla', exact: true }).click();
    await expect(page.getByText('Yayında', { exact: true })).toBeVisible();

    // Public adres, ayarlar ekranında yazılı olan slug'dan kuruluyor.
    const publicPath = await page
      .locator('a[href*="/status/"]')
      .first()
      .getAttribute('href');
    expect(publicPath).toBeTruthy();
    const slug = publicPath!.split('/status/')[1].split(/[?#]/)[0];

    // --- Ziyaretçi: her şey çalışıyor ----------------------------------------
    await page.goto(`/status/${slug}`);
    await expect(page.getByText('Tüm sistemler çalışıyor')).toBeVisible();
    await expect(page.getByText(monitorName)).toBeVisible();

    // --- HEDEF DÜŞÜYOR --------------------------------------------------------
    target.breakIt();

    /**
     * Ziyaretçi SAYFAYI YENİLEMİYOR: public sayfa 6.2'den beri canlı
     * (WebSocket olayı → `router.refresh()`), yani kesinti kendiliğinden
     * belirmeli. Bu bekleyiş aynı anda üç şeyi ölçüyor: worker'ın kontrolü,
     * durum makinesinin DOWN kararı ve kesinti defterinin açtığı kaydın
     * public sorgudan geçmesi.
     */
    await expect(page.getByText('Büyük kesinti')).toBeVisible({
      timeout: 90_000,
    });

    // Kesintinin BAŞLIĞI otomatik kaydın kendisi (4.1: monitor adını taşıyor).
    await expect(
      page.getByRole('heading', { name: new RegExp(monitorName) }),
    ).toBeVisible();

    // --- Pano: kesinti OTOMATİK açılmış --------------------------------------
    await page.goto('/incidents');
    await expect(
      page.getByRole('heading', { name: 'Kesintiler' }),
    ).toBeVisible();

    const incidentRow = page
      .getByRole('row', { name: new RegExp(monitorName) })
      .first();
    await expect(incidentRow).toBeVisible({ timeout: 30_000 });
    await expect(incidentRow.getByText('Otomatik')).toBeVisible();

    // Monitor da "Kesinti" gösteriyor: pano ile status sayfası aynı şeyi
    // söylüyor (3.3/4.1'in "durum tek yerden geçer" kuralı).
    await page.goto('/');
    await expect(
      page
        .getByRole('row', { name: new RegExp(monitorName) })
        .getByText('Kesinti'),
    ).toBeVisible({ timeout: 30_000 });
  } finally {
    await target.close();
  }
});
