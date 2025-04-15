<template>
  <v-container>
    <v-row>
      <v-col>
        <v-card>
          <v-card-title>Configuration</v-card-title>
          <v-card-text>
            <v-form @submit.prevent="saveConfig">
              <v-text-field v-model="config.url" label="WebDAV URL" required></v-text-field>
              <v-text-field v-model="config.user" label="Username" required></v-text-field>
              <v-text-field v-model="config.pass" label="Password" type="password" required></v-text-field>
              <v-text-field v-model="config.local_dir" label="Local Directory" required></v-text-field>
              <v-text-field v-model="config.remote_dir" label="Remote Directory" required></v-text-field>
              <v-select v-model="config.mode" :items="modes" label="Sync Mode" required></v-select>
              <v-btn color="primary" type="submit">Save</v-btn>
              <v-alert v-if="error" type="error">{{ error }}</v-alert>
              <v-alert v-if="success" type="success">Configuration saved</v-alert>
            </v-form>
          </v-card-text>
        </v-card>
      </v-col>
    </v-row>
  </v-container>
</template>

<script>
export default {
  data() {
    return {
      config: {
        url: '',
        user: '',
        pass: '',
        local_dir: '',
        remote_dir: '',
        mode: 'bidirectional',
      },
      modes: ['bidirectional', 'source-to-target', 'target-to-source'],
      error: '',
      success: false,
    }
  },
  mounted() {
    this.fetchConfig()
  },
  methods: {
    async fetchConfig() {
      const res = await fetch('/api/config', {
        headers: { Authorization: `Bearer ${localStorage.getItem('token')}` },
      })
      this.config = await res.json()
    },
    async saveConfig() {
      try {
        const res = await fetch('/api/config', {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json',
            Authorization: `Bearer ${localStorage.getItem('token')}`,
          },
          body: JSON.stringify(this.config),
        })
        if (res.ok) {
          this.success = true
          this.error = ''
        } else {
          this.error = (await res.json()).error
          this.success = false
        }
      } catch (err) {
        this.error = 'Failed to save configuration'
        this.success = false
      }
    },
  },
}
</script>