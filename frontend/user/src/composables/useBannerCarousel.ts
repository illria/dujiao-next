Warning: fs was declared with const; use let for reassignable variables.
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { bannerAPI } from '../api'
import { getImageUrl } from '../utils/image'
import { useLocalized } from './useProduct'
import fallbackHeroImage from '../assets/home-hero-fallback.png'

const FALLBACK_HOME_BANNER = {
  id: 'fallback-home-hero',
  position: 'home_hero',
  image: fallbackHeroImage,
  mobile_image: fallbackHeroImage,
  link_type: 'internal',
  link_value: '/products',
  open_in_new_tab: false,
}

export function useBannerCarousel() {
  const router = useRouter()
  const { t } = useI18n()
  const { getLocalizedText } = useLocalized()

  const banners = ref<any[]>([])
  const bannerLoading = ref(true)
  const currentBannerIndex = ref(0)
  let heroAutoPlayTimer: ReturnType<typeof setInterval> | null = null

  // Touch swipe
  let touchStartX = 0
  const onBannerTouchStart = (e: TouchEvent) => {
    touchStartX = e.touches[0]?.clientX ?? 0
  }
  const onBannerTouchEnd = (e: TouchEvent) => {
    const diff = touchStartX - (e.changedTouches[0]?.clientX ?? 0)
    if (Math.abs(diff) > 50) {
      if (diff > 0) handleNextHeroBanner()
      else handlePrevHeroBanner()
    }
  }

  const bannerCount = computed(() => banners.value.length)
  const showHeroSection = computed(() => bannerLoading.value || bannerCount.value > 0)

  const heroBanner = computed(() => {
    if (banners.value.length === 0) return null
    if (currentBannerIndex.value >= banners.value.length) return banners.value[0]
    return banners.value[currentBannerIndex.value]
  })

  const goToBanner = (index: number) => {
    if (banners.value.length === 0) return
    const total = banners.value.length
    currentBannerIndex.value = ((index % total) + total) % total
  }

  const nextHeroBanner = () => goToBanner(currentBannerIndex.value + 1)
  const prevHeroBanner = () => goToBanner(currentBannerIndex.value - 1)

  const stopHeroAutoPlay = () => {
    if (heroAutoPlayTimer) {
      clearInterval(heroAutoPlayTimer)
      heroAutoPlayTimer = null
    }
  }

  const startHeroAutoPlay = () => {
    stopHeroAutoPlay()
    if (banners.value.length <= 1) return
    heroAutoPlayTimer = setInterval(() => {
      nextHeroBanner()
    }, 5000)
  }

  const handleNextHeroBanner = () => {
    nextHeroBanner()
    startHeroAutoPlay()
  }

  const handlePrevHeroBanner = () => {
    prevHeroBanner()
    startHeroAutoPlay()
  }

  const selectHeroBanner = (index: number) => {
    goToBanner(index)
    startHeroAutoPlay()
  }

  const heroImage = computed(() => {
    const banner = heroBanner.value
    if (!banner) return ''
    const isMobile = typeof window !== 'undefined' && window.innerWidth < 768
    const imagePath = isMobile && banner.mobile_image
      ? banner.mobile_image
      : (banner.image || banner.mobile_image || '')
    if (imagePath === fallbackHeroImage) {
      return fallbackHeroImage
    }
    return getImageUrl(imagePath)
  })

  const heroBadge = computed(() => {
    if (!heroBanner.value) return t('home.hero.badge')
    if (heroBanner.value.id === FALLBACK_HOME_BANNER.id) return t('vault.hero.badge')
    return getLocalizedText(heroBanner.value.badge) || getLocalizedText(heroBanner.value.title) || t('home.hero.badge')
  })

  const heroTitle = computed(() => {
    if (!heroBanner.value) return t('home.hero.title')
    if (heroBanner.value.id === FALLBACK_HOME_BANNER.id) {
      return `${t('vault.hero.titleLead')}${t('vault.hero.titleHl')}`
    }
    return getLocalizedText(heroBanner.value.title) || t('home.hero.title')
  })

  const heroSubtitle = computed(() => {
    if (!heroBanner.value) return t('home.hero.subtitle')
    if (heroBanner.value.id === FALLBACK_HOME_BANNER.id) return t('vault.hero.lede')
    return getLocalizedText(heroBanner.value.subtitle) || t('home.hero.subtitle')
  })

  const heroLink = computed(() => {
    const banner = heroBanner.value
    if (!banner || banner.link_type === 'none') return ''
    return banner.link_value || ''
  })

  const hasHeroLink = computed(() => heroLink.value.trim().length > 0)

  const heroPrimaryButtonText = computed(() => {
    if (!hasHeroLink.value || heroLink.value === '/products') return t('home.hero.cta')
    const linkType = String(heroBanner.value?.link_type || '').toLowerCase()
    if (linkType === 'external') {
      return t('common.learnMore')
    }
    return t('common.viewDetails')
  })

  const heroOpenInNewTab = computed(() => Boolean(heroBanner.value?.open_in_new_tab))

  const goToHeroLink = () => {
    if (!heroLink.value) {
      router.push('/products')
      return
    }
    const isExternal = /^https?:\/\//i.test(heroLink.value)
    if (isExternal || heroOpenInNewTab.value) {
      window.open(heroLink.value, heroOpenInNewTab.value ? '_blank' : '_self')
      return
    }
    router.push(heroLink.value)
  }

  const loadBanners = async () => {
    bannerLoading.value = true
    try {
      const response = await bannerAPI.list({ position: 'home_hero', limit: 5 })
      const loadedBanners = response.data.data
      banners.value = Array.isArray(loadedBanners) && loadedBanners.length > 0
        ? loadedBanners
        : [FALLBACK_HOME_BANNER]
      currentBannerIndex.value = 0
      startHeroAutoPlay()
    } catch (error) {
      banners.value = [FALLBACK_HOME_BANNER]
      currentBannerIndex.value = 0
      startHeroAutoPlay()
      console.error('Failed to load banners:', error)
    } finally {
      bannerLoading.value = false
    }
  }

  return {
    banners,
    bannerLoading,
    currentBannerIndex,
    bannerCount,
    showHeroSection,
    heroBanner,
    heroImage,
    heroBadge,
    heroTitle,
    heroSubtitle,
    heroLink,
    hasHeroLink,
    heroPrimaryButtonText,
    heroOpenInNewTab,
    loadBanners,
    handleNextHeroBanner,
    handlePrevHeroBanner,
    selectHeroBanner,
    goToHeroLink,
    onBannerTouchStart,
    onBannerTouchEnd,
    stopHeroAutoPlay,
  }
}
