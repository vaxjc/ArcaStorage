# Arca

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24%2B-00ADD8.svg)](go.mod)

**Almacenamiento de objetos compatible con S3, en tu propio servidor.**

Arca es un servidor de archivos privado que habla la API de S3. Lo levantas con Docker, le das un volumen y tu aplicación sube, descarga y comparte archivos con el mismo código que usaría con S3, Cloudflare R2 o los buckets de Railway. Solo cambia el `endpoint`.

Todo cabe en un binario de Go sin dependencias: sin base de datos, sin consola web, sin servicios externos. Los archivos quedan en disco, en carpetas que puedes ver y copiar.

```text
tu app  ──  SDK de S3  ──>  Arca  ──>  /data
```

### Para qué sirve

- Adjuntos de un CRM o un ERP: contratos, facturas, fotos de clientes
- Avatares y archivos que suben los usuarios de tu app
- Exportaciones, reportes y backups generados por tus procesos
- Un S3 local para desarrollar sin tocar la nube

### Lo que trae

- **Compatible con los clientes que ya usas.** SDK de AWS para Node, Python, Go o PHP, la CLI `aws`, `rclone`.
- **Privado por defecto.** Toda petición va firmada. Para compartir un archivo generas una URL temporal.
- **Subidas directas desde el navegador**, sin que el archivo pase por tu backend.
- **Archivos grandes** con subida multipart y descargas por rangos.
- **Integridad verificada.** Una subida incompleta o con checksum distinto nunca reemplaza un archivo.
- **Listo para Easypanel y Docker**, con volumen persistente.

### Lo que no intenta ser

Arca corre en un solo nodo y usa un único par de claves. No replica datos, no tiene panel de usuarios ni versionado. Si necesitas un clúster distribuido, esta no es la herramienta. Si necesitas guardar archivos de una o varias apps sin montar infraestructura, sí.

## Estado

Arca es un proyecto joven. Cubre las operaciones de S3 que usa una aplicación típica y tiene tests para la firma, las subidas y los listados. No ha pasado una auditoría de seguridad externa. Úsalo para cargas pequeñas y medianas, y haz copias del volumen.

## Contenido

- [Inicio rápido](#inicio-rápido)
- [Conectar una aplicación](#conectar-una-aplicación)
- [Despliegue en Easypanel](#despliegue-en-easypanel)
- [Configuración](#configuración)
- [Operaciones soportadas](#operaciones-soportadas)
- [Seguridad](#seguridad)
- [Cómo guarda los datos](#cómo-guarda-los-datos)
- [Copias de seguridad](#copias-de-seguridad)
- [Límites conocidos](#límites-conocidos)
- [Problemas frecuentes](#problemas-frecuentes)
- [Desarrollo](#desarrollo)
- [Licencia](#licencia)

## Inicio rápido

### Con Docker

```bash
git clone <url-del-repo> arca
cd arca
docker compose up --build
```

La primera vez, Arca genera un par de claves y lo imprime en el log:

```text
arca-1  | arca listo
arca-1  |   endpoint    http://localhost:9000
arca-1  |   escuchando  0.0.0.0:9000
arca-1  |   region      us-east-1
arca-1  |   access key  ARCAQ7M2XK9PLR4TVN8HWD
arca-1  |   secret key  xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
arca-1  |   credenciales guardadas en /data/credentials.json
```

El secret solo se imprime en ese primer arranque. Guárdalo. Después queda en `/data/credentials.json`, dentro del volumen `arca-data`.

Si prefieres fijar tus propias claves, defínelas antes de arrancar:

```bash
export ARCA_ACCESS_KEY=mi-access-key
export ARCA_SECRET_KEY=$(openssl rand -base64 32)
docker compose up --build
```

### Sin Docker

Necesitas Go 1.24 o superior.

```bash
go build -o bin/arca ./cmd/arca
./bin/arca
```

Arca escucha en `http://127.0.0.1:9000` y guarda los datos en `./data`.

### Probar que responde

Con la CLI de AWS:

```bash
export AWS_ACCESS_KEY_ID=<access key>
export AWS_SECRET_ACCESS_KEY=<secret key>
export AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://localhost:9000

aws s3 mb s3://pruebas
echo "hola" > hola.txt
aws s3 cp hola.txt s3://pruebas/hola.txt
aws s3 ls s3://pruebas/
aws s3 presign s3://pruebas/hola.txt --expires-in 300
```

La última línea devuelve una URL temporal. Se puede abrir en el navegador o con `curl` sin credenciales hasta que caduque.

## Conectar una aplicación

Una aplicación necesita cinco datos:

| Dato | Ejemplo |
|---|---|
| Endpoint | `https://arca.tudominio.com` |
| Región | `us-east-1` (el valor de `ARCA_REGION`) |
| Bucket | `crm` |
| Access key | `ARCA...` |
| Secret key | solo en el servidor de la aplicación |

Usa siempre **path-style** (`https://arca.tudominio.com/crm/archivo.pdf`). El estilo virtual-hosted (`https://crm.arca.tudominio.com/...`) exige un DNS comodín que la mayoría de despliegues no tiene.

### Node.js

```bash
npm install @aws-sdk/client-s3 @aws-sdk/s3-request-presigner
```

```js
import {
  S3Client,
  PutObjectCommand,
  GetObjectCommand,
  DeleteObjectCommand,
} from "@aws-sdk/client-s3";
import { getSignedUrl } from "@aws-sdk/s3-request-presigner";

export const arca = new S3Client({
  endpoint: process.env.ARCA_ENDPOINT,
  region: process.env.ARCA_REGION ?? "us-east-1",
  forcePathStyle: true,
  credentials: {
    accessKeyId: process.env.ARCA_ACCESS_KEY,
    secretAccessKey: process.env.ARCA_SECRET_KEY,
  },
});

const Bucket = process.env.ARCA_BUCKET ?? "crm";

// Subida desde el servidor
await arca.send(
  new PutObjectCommand({
    Bucket,
    Key: "clientes/42/contrato.pdf",
    Body: buffer,
    ContentType: "application/pdf",
  })
);

// URL para que el navegador suba directo (5 minutos)
const uploadUrl = await getSignedUrl(
  arca,
  new PutObjectCommand({ Bucket, Key: "clientes/42/foto.jpg", ContentType: "image/jpeg" }),
  { expiresIn: 300 }
);

// URL de descarga temporal
const downloadUrl = await getSignedUrl(
  arca,
  new GetObjectCommand({ Bucket, Key: "clientes/42/contrato.pdf" }),
  { expiresIn: 300 }
);

// Borrar
await arca.send(new DeleteObjectCommand({ Bucket, Key: "clientes/42/contrato.pdf" }));
```
### Python

```python
import boto3
from botocore.config import Config

arca = boto3.client(
    "s3",
    endpoint_url="https://arca.tudominio.com",
    region_name="us-east-1",
    aws_access_key_id="ARCA...",
    aws_secret_access_key="...",
    config=Config(s3={"addressing_style": "path"}),
)

arca.upload_file("contrato.pdf", "crm", "clientes/42/contrato.pdf")

url = arca.generate_presigned_url(
    "get_object",
    Params={"Bucket": "crm", "Key": "clientes/42/contrato.pdf"},
    ExpiresIn=300,
)
```

### Subida desde el navegador

El navegador nunca recibe el secret. El flujo recomendado:

1. El usuario elige un archivo.
2. El frontend pide al backend una URL de subida para una clave concreta, por ejemplo `clientes/42/contrato.pdf`.
3. El backend comprueba permisos y devuelve una URL prefirmada de `PUT` válida por pocos minutos.
4. El navegador envía el archivo:

   ```js
   await fetch(uploadUrl, {
     method: "PUT",
     headers: { "Content-Type": file.type },
     body: file,
   });
   ```

5. El backend guarda la clave del objeto en su base de datos. El archivo no pasa por el backend ni por la base.

Para mostrar el archivo, el backend genera una URL de `GET` en el momento y redirige a ella, o descarga el objeto y lo entrega él mismo.

Si el SDK incluyó el `Content-Type` en la firma y el `fetch` envía otro, Arca responde `SignatureDoesNotMatch`. Envía el mismo tipo que usaste al generar la URL.

## Despliegue en Easypanel

Estos pasos sirven para un servicio de tipo **Compose** apuntando a este repositorio.

**1. Variables de entorno**

En la pestaña *Environment* del servicio:

```dotenv
ARCA_ACCESS_KEY=una-access-key-larga
ARCA_SECRET_KEY=un-secret-largo
ARCA_PUBLIC_URL=https://arca.tudominio.com
ARCA_REGION=us-east-1
```

Activa **Create .env file** y guarda. En un servicio Compose, Easypanel usa esas variables para rellenar el `docker-compose.yml`; el archivo del repositorio ya las pasa al contenedor.

Genera claves con:

```bash
echo "ARCA$(openssl rand -hex 10 | tr a-f A-F)"
openssl rand -base64 32
```

**2. Dominio**

En *Domains*, crea el dominio con HTTPS activado y este destino:

| Campo | Valor |
|---|---|
| Protocol | `HTTP` |
| Port | `9000` |
| Path | `/` |
| Compose Service | `arca` |

El puerto por defecto que propone Easypanel es `80`. Con ese valor el dominio devuelve `502`.

**3. Volumen**

En un servicio Compose, el volumen se declara en `docker-compose.yml` (`arca-data:/data`) y no aparece en la pestaña de almacenamiento del panel. Los datos sobreviven a cada deploy. Se pierden si borras el servicio o cambias el nombre del volumen.

**4. Deploy y comprobación**

Despliega y revisa el log. Debe mostrar tu dominio en `endpoint` y tu access key:

```text
arca listo
  endpoint    https://arca.tudominio.com
  escuchando  0.0.0.0:9000
  access key  una-access-key-larga
```

Después crea el bucket de la aplicación:

```bash
aws s3 mb s3://crm --endpoint-url https://arca.tudominio.com
```

## Configuración

Cada opción se puede pasar como variable de entorno o como flag. El flag tiene prioridad.

| Variable | Flag | Por defecto | Descripción |
|---|---|---|---|
| `ARCA_ADDR` | `--addr` | `127.0.0.1:9000` | Dirección de escucha |
| `PORT` | | | Si está definida y `ARCA_ADDR` no, escucha en `0.0.0.0:$PORT`. La imagen Docker usa `9000` |
| `ARCA_DATA` | `--data` | `data` (`/data` en Docker) | Directorio de objetos y credenciales |
| `ARCA_ACCESS_KEY` | `--access-key` | se genera | Access key |
| `ARCA_SECRET_KEY` | `--secret-key` | se genera | Secret key |
| `ARCA_PUBLIC_URL` | `--public-url` | `http://localhost:9000` | URL con la que los clientes llegan al servidor |
| `ARCA_REGION` | `--region` | `us-east-1` | Región que Arca informa a los clientes |
| `ARCA_TLS_CERT` | `--tls-cert` | | Certificado, si Arca termina TLS él mismo |
| `ARCA_TLS_KEY` | `--tls-key` | | Llave privada del certificado |
| | `--max-object-bytes` | `8589934592` (8 GiB) | Tamaño máximo de un objeto o una parte |

**Credenciales.** Si `ARCA_ACCESS_KEY` y `ARCA_SECRET_KEY` están definidas, Arca usa esas y no toca `credentials.json`. Si faltan las dos, lee `credentials.json` del directorio de datos, y si no existe lo crea. Definir solo una de las dos es un error.

**Rotar claves.** Cambia las dos variables y reinicia. Las URLs prefirmadas emitidas con la clave anterior dejan de funcionar.

**Nombres anteriores.** Las variables `STORAGE_*` de versiones previas siguen funcionando como respaldo de las `ARCA_*`.

## Operaciones soportadas

| Área | Operaciones |
|---|---|
| Buckets | `CreateBucket`, `DeleteBucket` (solo vacío), `HeadBucket`, `ListBuckets`, `GetBucketLocation` |
| Objetos | `PutObject`, `GetObject` (con `Range`), `HeadObject`, `DeleteObject`, `DeleteObjects`, `CopyObject` |
| Listados | `ListObjects` y `ListObjectsV2` con `prefix`, `delimiter`, `max-keys`, `start-after`, `continuation-token`, `encoding-type=url` |
| Multipart | `CreateMultipartUpload`, `UploadPart`, `CompleteMultipartUpload`, `AbortMultipartUpload`, `ListParts` |
| Metadatos | `Content-Type`, `Content-Disposition`, `Cache-Control`, `Content-Encoding`, `x-amz-meta-*` |
| Etiquetas | `GetObjectTagging`, `PutObjectTagging`, `DeleteObjectTagging` |
| CORS | `GetBucketCors`, `PutBucketCors`, `DeleteBucketCors` y respuestas a preflight |
| Firma | SigV4 en cabecera, URLs prefirmadas y cuerpos `aws-chunked` firmados y sin firmar |
| Integridad | `Content-MD5` y `x-amz-checksum-*` con CRC32, CRC32C, SHA-1 y SHA-256, también como trailer |
| Condicionales | `If-Match` e `If-None-Match` en subidas; los de lectura los resuelve el servidor HTTP |

Responden `501 NotImplemented`: versionado, ciclo de vida, políticas de bucket, cifrado del lado del servidor, replicación, notificaciones, object lock, website, logging e inventario.

`PutObjectAcl` se acepta y se ignora. `GetObjectAcl` devuelve siempre control total para el dueño. Todo es privado.

Las subidas por formulario HTML con **presigned POST** no están soportadas. Usa URLs prefirmadas de `PUT`.

## Seguridad

**Autenticación.** Cada petición debe llevar una firma AWS Signature V4 válida, en la cabecera `Authorization` o en la URL prefirmada. La firma se calcula con el secret, que nunca viaja por la red. Arca compara firmas en tiempo constante.

**Reloj.** Se rechazan peticiones firmadas con más de 15 minutos de diferencia respecto a la hora del servidor. Mantén el reloj del servidor sincronizado.

**URLs prefirmadas.** Duran lo que indique el cliente, con un máximo de 90 días. Cualquiera que tenga la URL puede usarla hasta que caduque, así que emítelas cortas.

**Rutas.** Las claves con `..`, `.`, segmentos vacíos, barras invertidas o bytes nulos se rechazan. Una clave no puede salir del directorio del bucket.

**Escrituras.** Cada subida se escribe primero en un archivo temporal. Solo se mueve a su sitio si el cuerpo llegó completo y los checksums coinciden. Una subida cortada no deja un objeto a medias.

**Red.** Sin `PORT` ni `ARCA_ADDR`, Arca solo escucha en `127.0.0.1`. En producción ponlo detrás de un proxy con HTTPS (Easypanel, Caddy, Traefik, nginx) o usa `ARCA_TLS_CERT` y `ARCA_TLS_KEY`.

**CORS.** Mientras un bucket no tenga configuración CORS propia, Arca acepta peticiones del navegador desde cualquier origen. Eso no da acceso sin firma, pero conviene limitarlo a tu dominio:

```bash
aws s3api put-bucket-cors --bucket crm --endpoint-url https://arca.tudominio.com \
  --cors-configuration '{
    "CORSRules": [{
      "AllowedOrigins": ["https://crm.tudominio.com"],
      "AllowedMethods": ["GET", "PUT"],
      "AllowedHeaders": ["*"],
      "ExposeHeaders": ["ETag"],
      "MaxAgeSeconds": 3000
    }]
  }'
```

**Lo que Arca no hace.** No cifra los archivos en disco: quien tenga acceso al volumen puede leerlos. No tiene usuarios ni permisos por carpeta: hay un único par de claves con acceso total, y los permisos por usuario los aplica tu aplicación antes de firmar URLs. No limita la tasa de peticiones.

**Antes de exponerlo a internet:**

- [ ] `ARCA_PUBLIC_URL` con `https` y el dominio real
- [ ] Claves largas y aleatorias, fuera del repositorio
- [ ] Volumen persistente en `/data` con copias de seguridad
- [ ] CORS limitado a tus orígenes si el navegador sube directo
- [ ] El secret solo en variables del servidor, nunca en el frontend

Si encuentras una vulnerabilidad, repórtala con un [security advisory privado](https://docs.github.com/es/code-security/security-advisories) en el repositorio en vez de abrir un issue público.

## Cómo guarda los datos

```text
data/
├── credentials.json         claves generadas (0600)
└── b/
    └── crm/
        ├── bucket.json      fecha de creación y reglas CORS
        ├── o/               bytes de cada objeto
        │   └── clientes/42/contrato.pdf.obj
        ├── m/               metadatos en JSON
        │   └── clientes/42/contrato.pdf.json
        └── u/               subidas multipart en curso
```

La estructura de carpetas refleja las claves. Los metadatos (tamaño, ETag, tipo de contenido, etiquetas, checksums) viven en un JSON al lado de cada objeto. No hay base de datos ni índice aparte.

El ETag de un objeto subido de una vez es el MD5 de su contenido. El de un objeto multipart sigue la convención de S3: MD5 de los MD5 de las partes, seguido de `-N`.

## Copias de seguridad

Todo el estado está en el directorio de datos. Copiarlo es copiar el servidor completo.

Con Docker Compose en local:

```bash
docker run --rm \
  -v arca_arca-data:/data:ro \
  -v "$PWD":/backup \
  alpine tar czf /backup/arca-$(date +%F).tar.gz -C /data .
```

El nombre real del volumen lleva el prefijo del proyecto Compose. Búscalo con `docker volume ls | grep arca-data`.

También puedes sincronizar un bucket con cualquier herramienta S3:

```bash
aws s3 sync s3://crm ./respaldo-crm --endpoint-url https://arca.tudominio.com
```

Para restaurar, detén Arca, extrae el archivo en el directorio de datos y vuelve a arrancar.

## Límites conocidos

- **Un nodo.** No hay replicación ni alta disponibilidad. Si el disco falla, los datos dependen de tus copias.
- **Escrituras por bucket.** Las subidas a un mismo bucket se procesan de una en una. Buckets distintos no se bloquean entre sí. Para un CRM es suficiente; para ingesta masiva en paralelo no.
- **Listados.** Cada listado recorre el bucket en disco. Con decenas de miles de objetos por bucket, los listados se vuelven lentos. Leer, subir y borrar un objeto no depende del tamaño del bucket.
- **Tamaños.** 8 GiB por objeto o parte, configurable. Hasta 10 000 partes por subida multipart y 10 etiquetas por objeto.
- **Sin versionado.** Subir una clave existente la reemplaza. Borrar es definitivo.

## Problemas frecuentes

| Síntoma | Causa probable | Solución |
|---|---|---|
| `502` al abrir el dominio | El proxy apunta a otro puerto | Destino del dominio en el puerto `9000` |
| `InvalidAccessKeyId` | El cliente envía otra access key, o el contenedor arrancó con claves generadas | Revisa la línea `access key` del log; en Compose pasa las variables al contenedor |
| `SignatureDoesNotMatch` | Secret incorrecto, `Content-Type` distinto del firmado o un proxy que reescribe la URL | Revisa el secret y que el proxy no cambie el path ni el host |
| `RequestTimeTooSkewed` | El reloj del cliente o del servidor está desfasado | Sincroniza con NTP |
| `NoSuchBucket` | El bucket no existe o el cliente usa virtual-hosted | Crea el bucket y activa `forcePathStyle` |
| El log dice `endpoint http://localhost:9000` en producción | `ARCA_PUBLIC_URL` no llegó al contenedor | En Easypanel activa *Create .env file* y redespliega |
| Los archivos desaparecen tras un deploy | No hay volumen en `/data` | Declara el volumen en el Compose |
| El navegador falla con un error de CORS | Regla CORS que no incluye tu origen o método | Revisa `PutBucketCors` o bórrala con `DeleteBucketCors` |

Arca no tiene endpoint de salud. Una petición sin firma devuelve `403` con la cabecera `Server: arca`, lo que confirma que el proceso responde.

Cada petición deja una línea en el log con su id, método, ruta, estado y duración:

```text
2026/09/25 20:07:19 rid=26f3de313e813586 GET / 403 1ms
```

El mismo id vuelve al cliente en la cabecera `x-amz-request-id`.

## Desarrollo

```text
cmd/arca/          arranque, flags, credenciales
internal/auth/     firma y verificación SigV4
internal/s3api/    rutas HTTP, XML de S3, cuerpos aws-chunked, CORS
internal/store/    almacenamiento en disco, multipart, metadatos
```

Arca solo usa la biblioteca estándar de Go.

```bash
go test ./...
go vet ./...
go build -o bin/arca ./cmd/arca
```

Los tests de `internal/auth` incluyen el vector de firma oficial de la documentación de S3. Los de `internal/s3api` levantan un servidor real en memoria y prueban subidas, listados, multipart, URLs prefirmadas y cuerpos `aws-chunked`.

Los pull requests son bienvenidos. Para cambios grandes, abre antes un issue y describe el caso de uso. Arca prefiere hacer pocas cosas bien: una función nueva tiene que servir a aplicaciones reales y no complicar el despliegue.

## Licencia

[MIT](LICENSE). Puedes usar, modificar y distribuir Arca, también en proyectos comerciales, siempre que conserves el aviso de copyright.
