import { request } from './api/client'
import type { Session } from './api/client'
import type { Grant } from './appLock'

type Descriptor = Omit<PublicKeyCredentialDescriptor, 'id'> & { id: string }
type Creation = Omit<PublicKeyCredentialCreationOptions, 'challenge' | 'user' | 'excludeCredentials'> & { challenge: string; user: Omit<PublicKeyCredentialUserEntity, 'id'> & { id: string }; excludeCredentials?: Descriptor[] }
type Assertion = Omit<PublicKeyCredentialRequestOptions, 'challenge' | 'allowCredentials'> & { challenge: string; allowCredentials?: Descriptor[] }
const decode = (value: string) => Uint8Array.from(atob(value.replace(/-/g, '+').replace(/_/g, '/')), char => char.charCodeAt(0))
const encode = (value: ArrayBuffer) => btoa(String.fromCharCode(...new Uint8Array(value))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')

export async function verifyPasskey(purpose: 'register' | 'enable' | 'unlock', password: string, signal: AbortSignal): Promise<Grant & { session?: NonNullable<Session> }> {
  document.documentElement.setAttribute('data-verifying', '')
  try {
    const options = await request<{ ceremonyId: string; publicKey: Creation | Assertion }>(`/lock/${purpose}/options`, { method: 'POST', body: JSON.stringify(purpose === 'register' ? { password } : {}), signal })
    let result: PublicKeyCredential | null
    if (purpose === 'register') {
      const value = options.publicKey as Creation
      result = await navigator.credentials.create({ signal, publicKey: { ...value, challenge: decode(value.challenge), user: { ...value.user, id: decode(value.user.id) }, excludeCredentials: value.excludeCredentials?.map(item => ({ ...item, id: decode(item.id) })) } }) as PublicKeyCredential | null
    } else {
      const value = options.publicKey as Assertion
      result = await navigator.credentials.get({ signal, publicKey: { ...value, challenge: decode(value.challenge), allowCredentials: value.allowCredentials?.map(item => ({ ...item, id: decode(item.id) })) } }) as PublicKeyCredential | null
    }
    if (!result) throw new Error('Verification was cancelled. You can try again or use your password.')
    const response = result.response
    const credential = {
      id: result.id, rawId: encode(result.rawId), type: result.type,
      authenticatorAttachment: result.authenticatorAttachment,
      clientExtensionResults: result.getClientExtensionResults(),
      response: response instanceof AuthenticatorAttestationResponse
        ? { clientDataJSON: encode(response.clientDataJSON), attestationObject: encode(response.attestationObject), transports: response.getTransports?.() ?? [] }
        : { clientDataJSON: encode(response.clientDataJSON), authenticatorData: encode((response as AuthenticatorAssertionResponse).authenticatorData), signature: encode((response as AuthenticatorAssertionResponse).signature), userHandle: (response as AuthenticatorAssertionResponse).userHandle ? encode((response as AuthenticatorAssertionResponse).userHandle!) : null },
    }
    return await request(`/lock/${purpose}/finish`, { method: 'POST', body: JSON.stringify({ ceremonyId: options.ceremonyId, credential }), signal })
  } catch (error) {
    if (error instanceof DOMException && ['NotAllowedError', 'AbortError'].includes(error.name)) throw new Error('Verification was cancelled. Try again or use your password.', { cause: error })
    throw error
  } finally {
    document.documentElement.removeAttribute('data-verifying')
  }
}
