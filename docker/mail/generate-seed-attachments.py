#!/usr/bin/env python3
"""Regenerate the attachment-bearing seed emails in ./seed (PDF, PNG and JPEG attachments).

The generated .eml files are committed; run this only when changing the fixtures:
    python3 docker/mail/generate-seed-attachments.py
PDFs and PNGs are built in pure Python; JPEGs use ImageMagick (`magick`/`convert`).
"""
import base64, os, shutil, struct, subprocess, tempfile, zlib

HERE = os.path.dirname(os.path.abspath(__file__))
SEED = os.path.join(HERE, "seed")


def pdf(title, lines):
    """Build a small, valid single-page PDF containing the given text lines."""
    def esc(s):
        return s.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)")
    text = ["BT", "/F1 20 Tf", "72 760 Td", f"({esc(title)}) Tj", "/F1 12 Tf", "0 -30 Td", "16 TL"]
    for line in lines:
        text.append(f"({esc(line)}) Tj T*")
    text.append("ET")
    stream = "\n".join(text).encode("latin-1")
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Contents 4 0 R "
        b"/Resources << /Font << /F1 5 0 R >> >> >>",
        b"<< /Length %d >>\nstream\n" % len(stream) + stream + b"\nendstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for i, body in enumerate(objs, 1):
        offsets.append(len(out))
        out += b"%d 0 obj\n" % i + body + b"\nendobj\n"
    xref = len(out)
    out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1)
    for off in offsets:
        out += b"%010d 00000 n \n" % off
    out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objs) + 1, xref)
    return bytes(out)


def raster(width, height, pixel):
    """Return rows of RGB bytes from a pixel(x, y) -> (r, g, b) function."""
    return [b"".join(bytes(pixel(x, y)) for x in range(width)) for y in range(height)]


def png(width, height, pixel):
    rows = raster(width, height, pixel)
    raw = b"".join(b"\x00" + r for r in rows)

    def chunk(kind, data):
        body = kind + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))


def jpeg(width, height, pixel):
    magick = shutil.which("magick") or shutil.which("convert")
    if not magick:
        raise SystemExit("ImageMagick (magick/convert) is required to generate JPEG fixtures")
    with tempfile.TemporaryDirectory() as d:
        ppm = os.path.join(d, "in.ppm")
        with open(ppm, "wb") as f:
            f.write(b"P6\n%d %d\n255\n" % (width, height) + b"".join(raster(width, height, pixel)))
        out = os.path.join(d, "out.jpg")
        subprocess.run([magick, ppm, "-quality", "80", out], check=True)
        return open(out, "rb").read()


def screenshot(x, y):  # a fake application window with an error banner
    if y < 24:
        return (45, 52, 90)
    if 70 < y < 110 and 30 < x < 290:
        return (214, 60, 60)
    return (240, 242, 248) if (x // 20 + y // 20) % 2 else (228, 232, 242)


def photo(x, y):  # smooth gradient
    return (min(255, 40 + x), min(255, 90 + y), 170)


def logo(x, y):
    return (79, 70, 229) if (x - 32) ** 2 + (y - 32) ** 2 < 28 ** 2 else (255, 255, 255)


def b64(data):
    enc = base64.b64encode(data).decode()
    return "\n".join(enc[i:i + 76] for i in range(0, len(enc), 76))


def attachment(boundary, data, ctype, name, disposition="attachment", cid=None):
    head = [f"--{boundary}", f'Content-Type: {ctype}; name="{name}"']
    if cid:
        head.append(f"Content-ID: <{cid}>")
    head += [f'Content-Disposition: {disposition}; filename="{name}"', "Content-Transfer-Encoding: base64", ""]
    return "\n".join(head) + "\n" + b64(data) + "\n"


def mail(path, headers, body_parts, boundary, subtype="mixed"):
    text = "\n".join(f"{k}: {v}" for k, v in headers.items())
    text += f'\nMIME-Version: 1.0\nContent-Type: multipart/{subtype}; boundary="{boundary}"\n\n'
    text += "".join(body_parts) + f"--{boundary}--\n"
    with open(os.path.join(SEED, path), "w", newline="\n") as f:
        f.write(text)


def plain(boundary, body):
    return f"--{boundary}\nContent-Type: text/plain; charset=utf-8\n\n{body}\n\n"


def main():
    # --- support@example.com ---------------------------------------------------------
    b = "mix-error-screenshots"
    mail("support@example.com/05-error-screenshots.eml", {
        "From": "Erin Engineer <erin@customer.example>", "To": "Customer Support <support@example.com>",
        "Subject": "Export fails with an error (screenshots attached)",
        "Date": "Wed, 14 Oct 2026 08:45:00 +0000", "Message-ID": "<error-screenshots-1@customer.example>",
    }, [
        plain(b, "Hi,\n\nWhen I try to export a report I get the error shown in the attached screenshots\n"
                 "(one PNG, one JPEG taken on my phone). Could you take a look?\n\nThanks,\nErin"),
        attachment(b, png(320, 200, screenshot), "image/png", "export-error.png"),
        attachment(b, jpeg(400, 300, photo), "image/jpeg", "phone-photo.jpg"),
    ], b)

    b = "mix-payment-receipt"
    mail("support@example.com/06-payment-receipt.eml", {
        "From": "Bob Builder <bob@customer.example>", "To": "Customer Support <support@example.com>",
        "Subject": "Payment receipt for INV-2026-0042",
        "Date": "Thu, 15 Oct 2026 09:10:00 +0000", "Message-ID": "<payment-receipt-1@customer.example>",
    }, [
        plain(b, "Hello,\n\nFor your records, the bank receipt for the corrected invoice is attached as a PDF.\n\n"
                 "Regards,\nBob Builder"),
        attachment(b, pdf("Payment receipt", [
            "Invoice: INV-2026-0042", "Amount: EUR 1,250.00", "Paid on: 2026-10-14",
            "Payer: Acme Construction", "Reference: corrected invoice"]),
            "application/pdf", "receipt-INV-2026-0042.pdf"),
    ], b)

    b = "rel-signed-contract"
    html = ('<html><body><p>Hi team,</p><p>Please find our signed support contract attached. '
            'Our logo is below for the website listing.</p><p><img src="cid:logo@acme" alt="Acme logo" '
            'width="64" height="64"></p><p>Best,<br>Carla</p></body></html>')
    text = "\n".join([
        "From: Carla Contract <carla@acme.example>", "To: Customer Support <support@example.com>",
        "Subject: Signed support contract",
        "Date: Fri, 16 Oct 2026 13:30:00 +0000", "Message-ID: <signed-contract-1@acme.example>",
        "MIME-Version: 1.0", f'Content-Type: multipart/mixed; boundary="{b}"', "",
        f"--{b}", 'Content-Type: multipart/related; boundary="rel-inner"', "",
        "--rel-inner", 'Content-Type: multipart/alternative; boundary="alt-inner"', "",
        "--alt-inner", "Content-Type: text/plain; charset=utf-8", "",
        "Hi team,\n\nPlease find our signed support contract attached.\n\nBest,\nCarla", "",
        "--alt-inner", "Content-Type: text/html; charset=utf-8", "", html, "",
        "--alt-inner--", "",
        attachment("rel-inner", png(64, 64, logo), "image/png", "logo.png", "inline", "logo@acme").rstrip("\n"),
        "--rel-inner--", "",
        attachment(b, pdf("Support contract", [
            "Customer: Acme Corp", "Plan: Premium support", "Term: 12 months",
            "Signed by: Carla Contract", "Date: 2026-10-16"]),
            "application/pdf", "support-contract-signed.pdf").rstrip("\n"),
        f"--{b}--", "",
    ])
    with open(os.path.join(SEED, "support@example.com/07-signed-contract.eml"), "w", newline="\n") as f:
        f.write(text)

    # --- sales@example.com -----------------------------------------------------------
    b = "mix-purchase-order"
    mail("sales@example.com/03-purchase-order.eml", {
        "From": "Dave Prospect <dave@prospect.example>", "To": "Sales <sales@example.com>",
        "Subject": "Purchase order PO-7781",
        "Date": "Mon, 12 Oct 2026 15:00:00 +0000", "Message-ID": "<purchase-order-1@prospect.example>",
    }, [
        plain(b, "Hello,\n\nWe would like to go ahead with 50 seats. The purchase order (PDF) and a scan of the\n"
                 "signed approval form (JPEG) are attached.\n\nKind regards,\nDave Prospect"),
        attachment(b, pdf("Purchase order PO-7781", [
            "Supplier: MiniCRM", "Seats: 50", "Term: 24 months", "Total: EUR 14,400.00",
            "Approved by: D. Prospect"]), "application/pdf", "PO-7781.pdf"),
        attachment(b, jpeg(480, 320, photo), "image/jpeg", "signed-approval-scan.jpg"),
    ], b)

    b = "mix-brand-assets"
    mail("sales@example.com/04-brand-assets.eml", {
        "From": "Mia Marketing <mia@partner.example>", "To": "Sales <sales@example.com>",
        "Subject": "Co-marketing: our logo and one-pager",
        "Date": "Tue, 13 Oct 2026 10:20:00 +0000", "Message-ID": "<brand-assets-1@partner.example>",
    }, [
        plain(b, "Hi,\n\nAs discussed, attached are our logo (PNG) and the partnership one-pager (PDF).\n\nCheers,\nMia"),
        attachment(b, png(64, 64, logo), "image/png", "partner-logo.png"),
        attachment(b, pdf("Partnership one-pager", [
            "Partner: Partner BV", "Focus: co-marketing webinar", "Proposed date: 2026-11-12",
            "Contact: mia@partner.example"]), "application/pdf", "partnership-one-pager.pdf"),
    ], b)


if __name__ == "__main__":
    main()
