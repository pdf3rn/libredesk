<template>
  <form @submit="onSubmit" class="space-y-6 w-full">
    <FormField v-slot="{ componentField }" name="name">
      <FormItem>
        <FormLabel>{{ $t('globals.terms.name') }}</FormLabel>
        <FormControl>
          <Input type="text" placeholder="" v-bind="componentField" />
        </FormControl>
        <FormMessage />
      </FormItem>
    </FormField>

    <FormField v-slot="{ componentField, handleChange }" name="enabled">
      <FormItem>
        <SwitchField
          :title="$t('globals.terms.enabled')"
          :checked="componentField.modelValue"
          @update:checked="handleChange"
        />
      </FormItem>
    </FormField>

    <FormField v-slot="{ componentField, handleChange }" name="prompt_tags_on_reply">
      <FormItem>
        <SwitchField
          :title="$t('admin.inbox.promptTagsOnReply')"
          :description="$t('admin.inbox.promptTagsOnReply.description')"
          :checked="componentField.modelValue"
          @update:checked="handleChange"
        />
      </FormItem>
    </FormField>

    <div class="max-w-xl">
      <FormField v-slot="{ componentField }" name="reopen_window_hours">
        <FormItem>
          <FormLabel>{{ $t('admin.inbox.whatsapp.reopenWindow') }}</FormLabel>
          <FormControl>
            <Input type="number" min="0" placeholder="48" v-bind="componentField" />
          </FormControl>
          <FormDescription>
            {{ $t('admin.inbox.whatsapp.reopenWindow.description') }}
          </FormDescription>
          <FormMessage />
        </FormItem>
      </FormField>
    </div>

    <div class="box p-4 space-y-4">
      <h3 class="ui-heading">{{ $t('admin.inbox.telegram.bot') }}</h3>
      <p v-if="botUsername" class="ui-description">@{{ botUsername }}</p>
      <div class="max-w-xl">
        <FormField v-slot="{ componentField }" name="config.bot_token">
          <FormItem>
            <FormLabel>{{ $t('admin.inbox.telegram.botToken') }}</FormLabel>
            <FormControl>
              <Input
                type="password"
                placeholder="123456789:AA..."
                autocomplete="off"
                v-bind="componentField"
              />
            </FormControl>
            <FormDescription>
              {{ $t('admin.inbox.telegram.botToken.description') }}
            </FormDescription>
            <FormMessage />
          </FormItem>
        </FormField>
      </div>
    </div>

    <div class="box p-4 space-y-4">
      <h3 class="ui-heading">{{ $t('admin.inbox.telegram.mode') }}</h3>
      <FormField v-slot="{ componentField }" name="config.mode">
        <FormItem>
          <FormControl>
            <RadioGroup v-bind="componentField">
              <div class="flex flex-col space-y-3">
                <div class="flex items-start space-x-3">
                  <RadioGroupItem id="tg-mode-polling" value="polling" class="mt-0.5" />
                  <div>
                    <Label for="tg-mode-polling">{{ $t('admin.inbox.telegram.mode.polling') }}</Label>
                    <p class="ui-description">
                      {{ $t('admin.inbox.telegram.mode.polling.description') }}
                    </p>
                  </div>
                </div>
                <div class="flex items-start space-x-3">
                  <RadioGroupItem id="tg-mode-webhook" value="webhook" class="mt-0.5" />
                  <div>
                    <Label for="tg-mode-webhook">{{ $t('admin.inbox.telegram.mode.webhook') }}</Label>
                    <p class="ui-description">
                      {{ $t('admin.inbox.telegram.mode.webhook.description') }}
                    </p>
                  </div>
                </div>
              </div>
            </RadioGroup>
          </FormControl>
          <FormMessage />
        </FormItem>
      </FormField>

      <div v-if="mode === 'polling'" class="max-w-xl">
        <FormField v-slot="{ componentField }" name="config.poll_timeout">
          <FormItem>
            <FormLabel>{{ $t('admin.inbox.telegram.pollTimeout') }}</FormLabel>
            <FormControl>
              <Input type="number" min="1" max="50" placeholder="30" v-bind="componentField" />
            </FormControl>
            <FormDescription>
              {{ $t('admin.inbox.telegram.pollTimeout.description') }}
            </FormDescription>
            <FormMessage />
          </FormItem>
        </FormField>
      </div>

      <div v-else class="max-w-xl space-y-2">
        <p class="ui-description flex items-start gap-1.5">
          <Lightbulb class="size-4 mt-0.5 shrink-0" />
          <span>{{ $t('admin.inbox.telegram.webhook.description') }}</span>
        </p>
        <div v-if="webhookURL" class="space-y-2">
          <Label class="text-muted-foreground">{{ $t('globals.terms.callbackURL') }}</Label>
          <div class="flex items-center gap-2">
            <Input :model-value="webhookURL" readonly class="font-mono text-xs" />
            <CopyButton :text="webhookURL" />
          </div>
        </div>
      </div>

      <div class="max-w-xl">
        <FormField v-slot="{ componentField }" name="config.api_url">
          <FormItem>
            <FormLabel>{{ $t('admin.inbox.telegram.apiURL') }}</FormLabel>
            <FormControl>
              <Input type="text" placeholder="https://api.telegram.org" v-bind="componentField" />
            </FormControl>
            <FormDescription>
              {{ $t('admin.inbox.telegram.apiURL.description') }}
            </FormDescription>
            <FormMessage />
          </FormItem>
        </FormField>
      </div>
    </div>

    <Button type="submit" :is-loading="isLoading" :disabled="isLoading">
      {{ submitLabel }}
    </Button>
  </form>
</template>

<script setup>
import { watch, computed } from 'vue'
import { useForm } from 'vee-validate'
import { toTypedSchema } from '@vee-validate/zod'
import * as z from 'zod'
import {
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  FormDescription
} from '@shared-ui/components/ui/form/index.js'
import { Input } from '@shared-ui/components/ui/input/index.js'
import { Label } from '@shared-ui/components/ui/label'
import { RadioGroup, RadioGroupItem } from '@shared-ui/components/ui/radio-group/index.js'
import SwitchField from '@shared-ui/components/SwitchField.vue'
import { Button } from '@shared-ui/components/ui/button/index.js'
import CopyButton from '@/components/button/CopyButton.vue'
import { Lightbulb } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'

const props = defineProps({
  initialValues: { type: Object, default: () => ({}) },
  submitForm: { type: Function, required: true },
  submitLabel: { type: String, default: '' },
  isNewForm: { type: Boolean, default: false },
  isLoading: { type: Boolean, default: false }
})

const { t } = useI18n()

const webhookURL = computed(() => props.initialValues?.webhook_url || '')
const botUsername = computed(() => props.initialValues?.config?.bot_username || '')

const submitLabel = computed(
  () =>
    props.submitLabel ||
    (props.isNewForm ? t('globals.messages.create') : t('globals.messages.save'))
)

const schema = z.object({
  name: z.string().min(1, t('globals.messages.required')),
  enabled: z.boolean().optional(),
  prompt_tags_on_reply: z.boolean().optional(),
  reopen_window_hours: z.coerce.number().int().min(0).optional(),
  config: z.object({
    bot_token: z.string().min(1, t('globals.messages.required')),
    mode: z.enum(['polling', 'webhook']),
    poll_timeout: z.coerce.number().int().min(1).max(50).optional(),
    api_url: z.string().optional()
  })
})

const form = useForm({
  validationSchema: toTypedSchema(schema),
  initialValues: {
    name: '',
    enabled: true,
    prompt_tags_on_reply: false,
    reopen_window_hours: 48,
    config: { bot_token: '', mode: 'polling', poll_timeout: 30, api_url: '' }
  }
})

const mode = computed(() => form.values.config?.mode)

const onSubmit = form.handleSubmit(async (values) => {
  await props.submitForm(values)
})

watch(
  () => props.initialValues,
  (newValues) => {
    if (Object.keys(newValues).length === 0) return
    form.setValues(
      {
        ...newValues,
        reopen_window_hours: newValues.reopen_window_hours ?? 0,
        config: {
          bot_token: newValues.config?.bot_token || '',
          mode: newValues.config?.mode === 'webhook' ? 'webhook' : 'polling',
          poll_timeout: newValues.config?.poll_timeout || 30,
          api_url: newValues.config?.api_url || ''
        }
      },
      false
    )
  },
  { deep: true, immediate: true }
)
</script>
