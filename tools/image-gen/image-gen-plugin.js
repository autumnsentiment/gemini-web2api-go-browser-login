(() => {
  const launcherId = 'new-api-image-gen-launcher'

  function ensureLauncher() {
    if (!document.body || document.getElementById(launcherId)) return

    const launcher = document.createElement('a')
    launcher.id = launcherId
    launcher.href = '/image-gen.html'
    launcher.target = '_blank'
    launcher.rel = 'noopener noreferrer'
    launcher.setAttribute('aria-label', '打开在线生图工作台')
    launcher.title = '打开在线生图工作台'

    const icon = document.createElement('span')
    icon.className = 'image-gen-launcher-icon'
    icon.setAttribute('aria-hidden', 'true')

    const label = document.createElement('span')
    label.textContent = '在线生图'

    launcher.append(icon, label)
    document.body.appendChild(launcher)
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', ensureLauncher, { once: true })
  } else {
    ensureLauncher()
  }
})()
